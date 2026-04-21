use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::agent::{Agent, Event, Message, MessageSink, Payload, Task};
use crate::config::{GatewayImConfig, MEDIA_DIR};
use crate::database::{Database, InboundMessage};
use crate::server::Server;

use super::Im;

pub(super) struct Feishu {
    agent: Arc<Agent>,
    db: Arc<Database>,
    cancel: CancellationToken,
    tx: mpsc::Sender<Message>,
    rx: Mutex<mpsc::Receiver<Message>>,
    feishu_client: feishu::Client,
    workspace_dir: PathBuf,
}

impl Feishu {
    pub fn new(
        agent: Arc<Agent>,
        db: Arc<Database>,
        cfg: &GatewayImConfig,
        workspace_dir: &Path,
    ) -> Arc<Self> {
        let (tx, rx) = mpsc::channel(64);

        // Ensure media directory exists.
        let media_dir = workspace_dir.join(MEDIA_DIR);
        if let Err(e) = std::fs::create_dir_all(&media_dir) {
            warn!(path = %media_dir.display(), error = %e, "failed to create media dir");
        }

        let feishu_client = if cfg.base_url == "https://open.feishu.cn" {
            feishu::Client::new(&cfg.app_id, &cfg.app_secret)
        } else {
            feishu::Client::with_base_url(&cfg.app_id, &cfg.app_secret, &cfg.base_url)
        };

        Arc::new(Feishu {
            agent,
            db,
            cancel: CancellationToken::new(),
            tx,
            rx: Mutex::new(rx),
            feishu_client,
            workspace_dir: workspace_dir.to_path_buf(),
        })
    }

    /// Main frontend loop: connects WS, dispatches inbound events,
    /// reconnects on disconnect. Outbound rendering runs in a spawned task.
    async fn run_inner(self: &Arc<Self>) {
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

    async fn stop_inner(&self) {
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

        // Persist: status=received.
        let sender_id = event_data
            .get("sender")
            .and_then(|s| s.get("sender_id"))
            .and_then(|s| s.get("open_id"))
            .and_then(|v| v.as_str())
            .unwrap_or("");
        // Parse Feishu create_time (milliseconds since epoch).
        let source_ts = envelope
            .header
            .as_ref()
            .and_then(|h| h.create_time.as_deref())
            .and_then(|t| t.parse::<i64>().ok())
            .and_then(chrono::DateTime::from_timestamp_millis);
        let db_msg_id = match self
            .db
            .messages
            .save_received(&InboundMessage {
                chat_id: &chat_id,
                content: &raw_content,
                source_im: "feishu",
                msg_type: &msg_type,
                sender_id,
                trigger_msg_id: &msg_id,
                source_ts,
            })
            .await
        {
            Ok(id) => {
                debug!(msg_id, db_msg_id = id, "message persisted");
                Some(id)
            }
            Err(e) => {
                warn!(msg_id, error = %e, "save_received failed, continuing without DB");
                None
            }
        };

        let (ready_tx, ready_rx) = oneshot::channel();

        let task = Task {
            chat_id: chat_id.clone(),
            msg_id: msg_id.clone(),
            db_msg_id,
            ready: ready_rx,
            frontend: Arc::clone(&self) as Arc<dyn MessageSink>,
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
            // Persist: status=ready with processed content.
            if let Some(db_id) = db_msg_id
                && let Err(e) = this
                    .db
                    .messages
                    .save_ready(db_id, &payload.content, &payload.file_paths)
                    .await
            {
                warn!(msg_id, error = %e, "save_ready failed");
            }
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
    ///
    /// Flow per message:
    /// 1. Immediately open a thinking card (user sees instant feedback)
    /// 2. Consume events → update card on each Reply
    /// 3. If events close without Reply → dismiss card
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
                    self.render_message(&api, &mut msg).await;
                }
                _ = self.cancel.cancelled() => {
                    info!("outbound render loop shutting down, draining remaining messages");
                    break;
                }
            }
        }
        // Drain remaining messages so their replies still get sent.
        while let Ok(mut msg) = rx.try_recv() {
            info!(msg_id = %msg.msg_id, "draining message");
            self.render_message(&api, &mut msg).await;
        }
        info!("outbound render loop stopped");
    }

    /// Render a single message: open thinking card → consume events → update card.
    async fn render_message(&self, api: &feishu::api::Api, msg: &mut Message) {
        // Default to Chinese (primary user). Full language detection would
        // require carrying the user's text through Message, deferred for now.
        let lang = "zh";

        // Step 1: Open thinking card immediately.
        let card_id = match api
            .reply_message(&msg.msg_id, "interactive", &thinking_card(lang))
            .await
        {
            Ok(id) => {
                debug!(msg_id = %msg.msg_id, card_id = %id, "thinking card opened");
                Some(id)
            }
            Err(e) => {
                warn!(msg_id = %msg.msg_id, error = %e, "failed to open thinking card");
                None
            }
        };

        // Step 2: Consume events and update card.
        let mut got_reply = false;
        while let Some(event) = msg.events.recv().await {
            match event {
                Event::Reply { text } => {
                    got_reply = true;
                    let card_json = markdown_card(&text);
                    if let Some(cid) = &card_id {
                        // Update the existing thinking card with the reply.
                        match api.update_message(cid, &card_json).await {
                            Ok(()) => {
                                info!(msg_id = %msg.msg_id, "reply updated on card");
                            }
                            Err(e) => {
                                warn!(msg_id = %msg.msg_id, error = %e, "card update failed, sending new reply");
                                // Fallback: send as plain text reply.
                                let content = serde_json::json!({"text": text}).to_string();
                                let _ = api.reply_message(&msg.msg_id, "text", &content).await;
                            }
                        }
                    } else {
                        // No card opened → send as new reply.
                        let content = serde_json::json!({"text": text}).to_string();
                        if let Err(e) = api.reply_message(&msg.msg_id, "text", &content).await {
                            warn!(msg_id = %msg.msg_id, error = %e, "reply failed");
                        }
                    }
                }
            }
        }

        // Step 3: If events closed without any Reply, dismiss the thinking card.
        if !got_reply && let Some(cid) = &card_id {
            let _ = api.update_message(cid, &markdown_card("✓")).await;
        }

        debug!(msg_id = %msg.msg_id, "message rendering complete");
    }
}

#[async_trait::async_trait]
impl Server for Feishu {
    async fn run(self: Arc<Self>) {
        self.run_inner().await;
    }

    async fn stop(&self) {
        self.stop_inner().await;
    }
}

#[async_trait::async_trait]
impl MessageSink for Feishu {
    async fn submit_message(&self, msg: Message) {
        // Push to outbound render queue. The outbound loop consumes these.
        if self.tx.send(msg).await.is_err() {
            warn!("outbound channel closed, dropping message");
        }
    }
}

impl Im for Feishu {}

// --- Feishu card builders ---

/// Build a Feishu interactive card with markdown content.
fn markdown_card(md: &str) -> String {
    serde_json::json!({
        "schema": "2.0",
        "config": {"update_multi": true},
        "body": {
            "elements": [{"tag": "markdown", "content": md}]
        }
    })
    .to_string()
}

/// Build a "thinking" status card.
fn thinking_card(lang: &str) -> String {
    let text = if lang == "zh" {
        "💭 正在思考..."
    } else {
        "💭 Thinking..."
    };
    markdown_card(text)
}

/// Simple language detection: check for CJK characters.
#[allow(dead_code)]
fn detect_lang(text: &str) -> &'static str {
    for c in text.chars() {
        if ('\u{4E00}'..='\u{9FFF}').contains(&c) {
            return "zh";
        }
    }
    "en"
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
