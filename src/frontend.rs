use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::agent::{Agent, Event, Message, Payload, Task};
use crate::config::MEDIA_DIR;

pub struct Feishu {
    agent: Arc<Agent>,
    cancel: CancellationToken,
    tx: mpsc::Sender<Message>,
    rx: Mutex<mpsc::Receiver<Message>>,
    feishu_client: feishu::Client,
    workspace_dir: PathBuf,
}

impl Feishu {
    pub fn new(
        agent: Arc<Agent>,
        app_id: &str,
        app_secret: &str,
        workspace_dir: &Path,
    ) -> Arc<Self> {
        let (tx, rx) = mpsc::channel(64);

        // Ensure media directory exists.
        let media_dir = workspace_dir.join(MEDIA_DIR);
        if let Err(e) = std::fs::create_dir_all(&media_dir) {
            warn!(path = %media_dir.display(), error = %e, "failed to create media dir");
        }

        Arc::new(Feishu {
            agent,
            cancel: CancellationToken::new(),
            tx,
            rx: Mutex::new(rx),
            feishu_client: feishu::Client::new(app_id, app_secret),
            workspace_dir: workspace_dir.to_path_buf(),
        })
    }

    /// Main frontend loop: connects WS, dispatches inbound events,
    /// reconnects on disconnect. Outbound rendering runs in a spawned task.
    pub async fn run(self: &Arc<Self>) {
        info!("feishu frontend started");

        let outbound_handle = {
            let this = Arc::clone(self);
            tokio::spawn(async move { this.handle_outbound().await })
        };

        // Inbound: connect WS, receive events, reconnect on disconnect.
        loop {
            let mut ws = match self.feishu_client.connect_ws().await {
                Ok(ws) => {
                    info!("feishu WS connected");
                    ws
                }
                Err(e) => {
                    if e.is_fatal() {
                        warn!(error = %e, "feishu WS fatal error, giving up");
                        self.cancel.cancel();
                        break;
                    }
                    warn!(error = %e, "feishu WS connect failed, retrying in 5s");
                    tokio::select! {
                        _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => continue,
                        _ = self.cancel.cancelled() => break,
                    }
                }
            };

            loop {
                tokio::select! {
                    event = ws.next_event() => {
                        let Some(event) = event else {
                            info!("feishu WS disconnected, will reconnect");
                            break;
                        };
                        let this = Arc::clone(self);
                        tokio::spawn(async move { this.handle_inbound(event).await });
                    }
                    _ = self.cancel.cancelled() => {
                        info!("feishu frontend received shutdown signal");
                        let _ = outbound_handle.await;
                        info!("feishu frontend stopped");
                        return;
                    }
                }
            }
        }

        let _ = outbound_handle.await;
        info!("feishu frontend stopped");
    }

    pub async fn stop(&self) {
        self.cancel.cancel();
    }

    /// Inbound: parse a Feishu WS event, construct a Task with a oneshot
    /// ReadyCh, submit to Agent, and spawn media processing.
    async fn handle_inbound(self: Arc<Self>, event: feishu::types::FeishuEvent) {
        let feishu::types::FeishuEvent::Message(envelope) = event else {
            debug!("ignoring non-message event");
            return;
        };

        let header = match &envelope.header {
            Some(h) => h,
            None => return,
        };
        if header.event_type.as_deref() != Some("im.message.receive_v1") {
            debug!(
                event_type = header.event_type.as_deref().unwrap_or(""),
                "ignoring non-IM event"
            );
            return;
        }

        let event_data = match &envelope.event {
            Some(data) => data,
            None => return,
        };
        let message = match event_data.get("message") {
            Some(m) => m,
            None => {
                warn!("inbound event missing 'message' field");
                return;
            }
        };

        let msg_id = message
            .get("message_id")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        let chat_id = message
            .get("chat_id")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        if msg_id.is_empty() || chat_id.is_empty() {
            warn!(msg_id, chat_id, "inbound event missing msg_id or chat_id");
            return;
        }

        let msg_id = msg_id.to_string();
        let chat_id = chat_id.to_string();
        let msg_type = message
            .get("message_type")
            .and_then(|v| v.as_str())
            .unwrap_or("text")
            .to_string();
        let raw_content = message
            .get("content")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();

        info!(msg_id, chat_id, msg_type, "inbound message received");

        let (ready_tx, ready_rx) = oneshot::channel();

        let task = Task {
            chat_id: chat_id.clone(),
            msg_id: msg_id.clone(),
            ready: ready_rx,
            frontend: self.tx.clone(),
        };
        if let Err(e) = self.agent.submit_task(task).await {
            warn!(msg_id, chat_id, error = %e, "submit_task failed");
            return;
        }
        debug!(msg_id, chat_id, "task submitted to agent");

        // Spawn media processing: download files, extract text by msg_type.
        let this = Arc::clone(&self);
        tokio::spawn(async move {
            let payload = this.process_message(&msg_id, &msg_type, &raw_content).await;
            debug!(
                msg_id,
                chat_id,
                content_len = payload.content.len(),
                files = payload.file_paths.len(),
                "payload ready"
            );
            let _ = ready_tx.send(payload);
        });
    }

    /// Process a Feishu message by type: extract text, download media files.
    async fn process_message(&self, msg_id: &str, msg_type: &str, raw_content: &str) -> Payload {
        match msg_type {
            "text" => self.process_text(raw_content),
            "image" => self.process_image(msg_id, raw_content).await,
            "audio" => self.process_audio(msg_id, raw_content).await,
            "file" => self.process_file(msg_id, raw_content).await,
            "post" => self.process_post(msg_id, raw_content).await,
            other => {
                debug!(msg_type = other, "unknown message type, treating as text");
                self.process_text(raw_content)
            }
        }
    }

    fn process_text(&self, raw: &str) -> Payload {
        Payload {
            content: extract_text(raw),
            file_paths: vec![],
        }
    }

    async fn process_image(&self, msg_id: &str, raw: &str) -> Payload {
        let image_key = serde_json::from_str::<serde_json::Value>(raw)
            .ok()
            .and_then(|v| {
                v.get("image_key")
                    .and_then(|k| k.as_str())
                    .map(String::from)
            });

        let Some(image_key) = image_key else {
            return Payload {
                content: "The user sent an image.".into(),
                file_paths: vec![],
            };
        };

        match self
            .download_and_save(msg_id, &image_key, "image", ".png")
            .await
        {
            Ok(rel_path) => Payload {
                content: "The user sent an image.".into(),
                file_paths: vec![rel_path],
            },
            Err(e) => {
                warn!(msg_id, image_key, error = %e, "image download failed");
                Payload {
                    content: "The user sent an image that could not be downloaded.".into(),
                    file_paths: vec![],
                }
            }
        }
    }

    async fn process_audio(&self, msg_id: &str, raw: &str) -> Payload {
        let parsed = serde_json::from_str::<serde_json::Value>(raw).ok();
        let file_key = parsed
            .as_ref()
            .and_then(|v| v.get("file_key").and_then(|k| k.as_str()));
        let duration = parsed
            .as_ref()
            .and_then(|v| v.get("duration").and_then(|d| d.as_i64()))
            .unwrap_or(0);

        let Some(file_key) = file_key else {
            return Payload {
                content: "The user sent a voice message.".into(),
                file_paths: vec![],
            };
        };

        // Download audio file. Transcription is not yet implemented.
        match self
            .download_and_save(msg_id, file_key, "file", ".opus")
            .await
        {
            Ok(rel_path) => {
                let duration_s = duration / 1000;
                Payload {
                    content: format!(
                        "[Voice message, {}s, saved at {}]\n(transcription not yet available)",
                        duration_s, rel_path
                    ),
                    file_paths: vec![rel_path],
                }
            }
            Err(e) => {
                warn!(msg_id, file_key, error = %e, "audio download failed");
                Payload {
                    content: format!(
                        "The user sent a voice message ({}s, download failed).",
                        duration / 1000
                    ),
                    file_paths: vec![],
                }
            }
        }
    }

    async fn process_file(&self, msg_id: &str, raw: &str) -> Payload {
        let parsed = serde_json::from_str::<serde_json::Value>(raw).ok();
        let file_key = parsed
            .as_ref()
            .and_then(|v| v.get("file_key").and_then(|k| k.as_str()));
        let file_name = parsed
            .as_ref()
            .and_then(|v| v.get("file_name").and_then(|n| n.as_str()))
            .unwrap_or("unknown");

        let Some(file_key) = file_key else {
            return Payload {
                content: "The user sent a file.".into(),
                file_paths: vec![],
            };
        };

        let ext = Path::new(file_name)
            .extension()
            .and_then(|e| e.to_str())
            .map(|e| format!(".{e}"))
            .unwrap_or_else(|| ".bin".into());

        match self.download_and_save(msg_id, file_key, "file", &ext).await {
            Ok(rel_path) => {
                let abs_path = self.workspace_dir.join(&rel_path);
                Payload {
                    content: format!(
                        "The user sent a file '{}' (saved at {}, absolute path: {}).",
                        file_name,
                        rel_path,
                        abs_path.display()
                    ),
                    file_paths: vec![rel_path],
                }
            }
            Err(e) => {
                warn!(msg_id, file_key, file_name, error = %e, "file download failed");
                Payload {
                    content: format!("The user sent a file '{}' (download failed).", file_name),
                    file_paths: vec![],
                }
            }
        }
    }

    async fn process_post(&self, msg_id: &str, raw: &str) -> Payload {
        let parsed = match serde_json::from_str::<serde_json::Value>(raw) {
            Ok(v) => v,
            Err(_) => {
                return Payload {
                    content: raw.to_string(),
                    file_paths: vec![],
                };
            }
        };

        let (text, image_keys) = extract_post_content(&parsed);

        // Download all embedded images.
        let mut file_paths = Vec::new();
        for key in &image_keys {
            match self.download_and_save(msg_id, key, "image", ".png").await {
                Ok(rel_path) => file_paths.push(rel_path),
                Err(e) => warn!(msg_id, image_key = key, error = %e, "post image download failed"),
            }
        }

        let content = if text.is_empty() {
            "The user sent images.".into()
        } else {
            text
        };

        Payload {
            content,
            file_paths,
        }
    }

    /// Download a resource from Feishu API and save to workspace media dir.
    /// Returns the relative path (e.g. ".files/key.png").
    async fn download_and_save(
        &self,
        msg_id: &str,
        file_key: &str,
        resource_type: &str,
        ext: &str,
    ) -> Result<String, feishu::types::Error> {
        // Sanitize file_key before any I/O: reject path traversal attempts.
        if file_key.contains('/')
            || file_key.contains('\\')
            || file_key.contains("..")
            || file_key.is_empty()
        {
            return Err(feishu::types::Error::Connection(format!(
                "invalid file_key: {file_key}"
            )));
        }

        let api = self.feishu_client.api();
        let data = api
            .download_resource(msg_id, file_key, resource_type)
            .await?;

        // Sanity check: empty or JSON error response.
        if data.is_empty() {
            return Err(feishu::types::Error::Connection("empty response".into()));
        }
        if data.len() < 1000 && data.first() == Some(&b'{') {
            let msg = String::from_utf8_lossy(&data);
            return Err(feishu::types::Error::Connection(format!(
                "API error: {msg}"
            )));
        }

        let filename = format!("{file_key}{ext}");
        let abs_path = self.workspace_dir.join(MEDIA_DIR).join(&filename);
        tokio::fs::write(&abs_path, &data).await.map_err(|e| {
            feishu::types::Error::Connection(format!("save file {}: {e}", abs_path.display()))
        })?;

        let rel_path = format!("{MEDIA_DIR}/{filename}");
        info!(
            file_key,
            rel_path,
            size = data.len(),
            "media file downloaded"
        );
        Ok(rel_path)
    }

    /// Outbound render loop: receives Messages from the Agent, consumes
    /// their event streams, and replies via Feishu REST API.
    async fn handle_outbound(self: &Arc<Self>) {
        info!("outbound render loop started");
        let mut rx = self.rx.lock().await;
        let api = self.feishu_client.api();
        loop {
            tokio::select! {
                msg = rx.recv() => {
                    let Some(mut msg) = msg else {
                        debug!("outbound channel closed");
                        break;
                    };
                    info!(msg_id = %msg.msg_id, chat_id = %msg.chat_id, "rendering message");

                    while let Some(event) = msg.events.recv().await {
                        match event {
                            Event::Reply { text } => {
                                debug!(msg_id = %msg.msg_id, reply_len = text.len(), "sending reply");
                                let content = serde_json::json!({"text": text}).to_string();
                                match api.reply_message(&msg.msg_id, "text", &content).await {
                                    Ok(reply_id) => {
                                        info!(msg_id = %msg.msg_id, reply_id, "reply sent");
                                    }
                                    Err(e) => {
                                        warn!(msg_id = %msg.msg_id, error = %e, "reply failed");
                                    }
                                }
                            }
                        }
                    }
                    debug!(msg_id = %msg.msg_id, "message events exhausted, card closed");
                }
                _ = self.cancel.cancelled() => {
                    info!("outbound render loop shutting down, draining remaining messages");
                    break;
                }
            }
        }
        while let Ok(mut msg) = rx.try_recv() {
            info!(msg_id = %msg.msg_id, "draining message");
            while let Some(event) = msg.events.recv().await {
                match event {
                    Event::Reply { text } => {
                        let content = serde_json::json!({"text": text}).to_string();
                        if let Err(e) = api.reply_message(&msg.msg_id, "text", &content).await {
                            warn!(msg_id = %msg.msg_id, error = %e, "drain reply failed");
                        }
                    }
                }
            }
        }
        info!("outbound render loop stopped");
    }
}

// --- Content extraction helpers ---

/// Extract text from Feishu text message content JSON: `{"text":"..."}`.
fn extract_text(raw: &str) -> String {
    serde_json::from_str::<serde_json::Value>(raw)
        .ok()
        .and_then(|v| v.get("text").and_then(|t| t.as_str()).map(String::from))
        .unwrap_or_else(|| raw.to_string())
}

/// Extract text + image keys from a Feishu rich-text (post) message.
/// Handles both direct `{title, content}` and language-keyed `{zh_cn: {title, content}}` formats.
fn extract_post_content(parsed: &serde_json::Value) -> (String, Vec<String>) {
    // Try direct format first.
    if let Some(content) = parsed.get("content") {
        let title = parsed.get("title").and_then(|t| t.as_str()).unwrap_or("");
        let (body, images) = parse_post_content_array(content);
        return (join_text(title, &body), images);
    }

    // Try language-keyed format: {"zh_cn": {"title": "...", "content": [...]}}
    if let Some(obj) = parsed.as_object() {
        for (_lang, body) in obj {
            if let Some(content) = body.get("content") {
                let title = body.get("title").and_then(|t| t.as_str()).unwrap_or("");
                let (text, images) = parse_post_content_array(content);
                return (join_text(title, &text), images);
            }
        }
    }

    (String::new(), vec![])
}

/// Parse the `content` array of a post message: `[[{tag, text, image_key}, ...], ...]`
fn parse_post_content_array(content: &serde_json::Value) -> (String, Vec<String>) {
    let Some(lines) = content.as_array() else {
        return (String::new(), vec![]);
    };

    let mut texts = Vec::new();
    let mut images = Vec::new();

    for line in lines {
        let Some(elements) = line.as_array() else {
            continue;
        };
        let mut line_texts = Vec::new();
        for elem in elements {
            match elem.get("tag").and_then(|t| t.as_str()) {
                Some("text") => {
                    if let Some(text) = elem.get("text").and_then(|t| t.as_str())
                        && !text.is_empty()
                    {
                        line_texts.push(text.to_string());
                    }
                }
                Some("img") => {
                    if let Some(key) = elem.get("image_key").and_then(|k| k.as_str())
                        && !key.is_empty()
                    {
                        images.push(key.to_string());
                    }
                }
                _ => {}
            }
        }
        if !line_texts.is_empty() {
            texts.push(line_texts.join(""));
        }
    }

    (texts.join("\n"), images)
}

fn join_text(title: &str, body: &str) -> String {
    let title = title.trim();
    let body = body.trim();
    match (title.is_empty(), body.is_empty()) {
        (false, false) => format!("{title}\n{body}"),
        (false, true) => title.to_string(),
        _ => body.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extract_text_from_feishu_json() {
        assert_eq!(extract_text(r#"{"text":"hello"}"#), "hello");
    }

    #[test]
    fn extract_text_plain_fallback() {
        assert_eq!(extract_text("plain text"), "plain text");
    }

    #[test]
    fn extract_text_empty_object() {
        assert_eq!(extract_text("{}"), "{}");
    }

    #[test]
    fn extract_text_with_at_mention() {
        assert_eq!(
            extract_text(r#"{"text":"hello @_user_1 world"}"#),
            "hello @_user_1 world"
        );
    }

    #[test]
    fn post_content_direct_format() {
        let json = serde_json::json!({
            "title": "My Title",
            "content": [
                [{"tag": "text", "text": "line1 "},{"tag": "text", "text": "cont"}],
                [{"tag": "text", "text": "line2"}],
                [{"tag": "img", "image_key": "img_key_1"}]
            ]
        });
        let (text, images) = extract_post_content(&json);
        assert_eq!(text, "My Title\nline1 cont\nline2");
        assert_eq!(images, vec!["img_key_1"]);
    }

    #[test]
    fn post_content_language_keyed() {
        let json = serde_json::json!({
            "zh_cn": {
                "title": "标题",
                "content": [
                    [{"tag": "text", "text": "正文"}],
                    [{"tag": "img", "image_key": "img_a"}, {"tag": "img", "image_key": "img_b"}]
                ]
            }
        });
        let (text, images) = extract_post_content(&json);
        assert_eq!(text, "标题\n正文");
        assert_eq!(images, vec!["img_a", "img_b"]);
    }

    #[test]
    fn post_content_empty() {
        let json = serde_json::json!({});
        let (text, images) = extract_post_content(&json);
        assert!(text.is_empty());
        assert!(images.is_empty());
    }

    #[test]
    fn join_text_both() {
        assert_eq!(join_text("title", "body"), "title\nbody");
    }

    #[test]
    fn join_text_title_only() {
        assert_eq!(join_text("title", ""), "title");
    }

    #[test]
    fn join_text_body_only() {
        assert_eq!(join_text("", "body"), "body");
    }
}
