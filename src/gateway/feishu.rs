use std::collections::HashSet;
use std::future::Future;
use std::path::{Path, PathBuf};
use std::pin::Pin;

use futures::StreamExt;
use futures::stream::FuturesUnordered;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::agent::{Event, Message, Notification, Payload};
use crate::config::{GatewayImConfig, MEDIA_DIR};
use crate::database::{Database, InboundMessage};

/// Pending media work returned by the fast inbound path.
struct MediaWork {
    msg_id: String,
    msg_type: String,
    raw_content: String,
    db_msg_id: Option<i64>,
    ready_tx: oneshot::Sender<Payload>,
}

pub(super) struct Feishu<'a> {
    task_tx: mpsc::Sender<Message>,
    db: &'a Database,
    tx: mpsc::Sender<Notification>,
    rx: Mutex<mpsc::Receiver<Notification>>,
    feishu_client: feishu::Client,
    workspace_dir: PathBuf,
    transcriber: Option<super::transcribe::TranscribeClient>,
    /// Channel for receiving recovery messages from Recovery module.
    recover_rx: Mutex<mpsc::Receiver<crate::database::messages::UnrepliedMessage>>,
    /// Dedup inbound events by message_id. Feishu may deliver duplicates.
    seen_msg_ids: std::sync::Mutex<HashSet<String>>,
}

impl<'a> Feishu<'a> {
    pub(super) fn new(
        task_tx: mpsc::Sender<Message>,
        db: &'a Database,
        cfg: &GatewayImConfig,
        workspace_dir: &Path,
        transcriber: Option<super::transcribe::TranscribeClient>,
        recover_rx: mpsc::Receiver<crate::database::messages::UnrepliedMessage>,
    ) -> Self {
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

        Feishu {
            task_tx,
            db,
            tx,
            rx: Mutex::new(rx),
            feishu_client,
            workspace_dir: workspace_dir.to_path_buf(),
            transcriber,
            recover_rx: Mutex::new(recover_rx),
            seen_msg_ids: std::sync::Mutex::new(HashSet::new()),
        }
    }

    /// Single event loop: WS inbound + outbound rendering + media processing + cancel.
    pub(super) async fn run(&self, cancel: &CancellationToken) {
        info!("feishu frontend started");

        let mut rx = self.rx.lock().await;
        let mut recover_rx = self.recover_rx.lock().await;
        let api = self.feishu_client.api();
        // All async work (media download + outbound render) goes into one
        // FuturesUnordered so the select loop drives them all concurrently.
        // This prevents deadlocks from shared auth token refresh.
        let mut tasks: FuturesUnordered<Pin<Box<dyn Future<Output = ()> + Send + '_>>> =
            FuturesUnordered::new();

        loop {
            // Connect (or reconnect) WS. In-flight tasks survive reconnects.
            let mut ws = loop {
                match self.feishu_client.connect_ws().await {
                    Ok(ws) => {
                        info!("feishu WS connected");
                        break ws;
                    }
                    Err(e) => {
                        if e.is_fatal() {
                            warn!(error = %e, "feishu WS fatal error, giving up");
                            cancel.cancel();
                            return;
                        }
                        warn!(error = %e, "feishu WS connect failed, retrying in 5s");
                        tokio::select! {
                            _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => continue,
                            _ = cancel.cancelled() => return,
                        }
                    }
                }
            };

            loop {
                tokio::select! {
                    event = ws.next_event() => {
                        let Some(event) = event else {
                            info!("feishu WS disconnected, will reconnect");
                            break; // outer loop reconnects
                        };
                        if let Some(work) = self.handle_inbound(event).await {
                            tasks.push(Box::pin(self.process_media(work)));
                        }
                    }
                    notif = rx.recv() => {
                        let Some(notif) = notif else {
                            debug!("outbound channel closed");
                            return;
                        };
                        debug!(msg_id = %notif.msg_id, "outbound notification received, rendering");
                        tasks.push(Box::pin(self.render_one(&api, notif)));
                    }
                    recover = recover_rx.recv() => {
                        if let Some(msg) = recover {
                            tasks.push(Box::pin(self.handle_recover(msg)));
                        }
                    }
                    _ = tasks.next(), if !tasks.is_empty() => {}
                    _ = cancel.cancelled() => {
                        info!("feishu frontend shutting down, draining outbound");
                        while let Ok(Some(notif)) =
                            tokio::time::timeout(
                                std::time::Duration::from_millis(500),
                                rx.recv(),
                            ).await
                        {
                            tasks.push(Box::pin(self.render_one(&api, notif)));
                        }
                        // Drain remaining tasks.
                        while tasks.next().await.is_some() {}
                        info!("feishu frontend stopped");
                        return;
                    }
                }
            }
        }
    }

    /// Handle a recovery message: reprocess if needed, then submit to agent.
    async fn handle_recover(&self, msg: crate::database::messages::UnrepliedMessage) {
        info!(
            db_msg_id = msg.db_msg_id,
            msg_id = msg.trigger_msg_id,
            ready = msg.ready,
            "recovering message"
        );

        let payload = if msg.ready {
            // Content already processed — use as-is.
            Payload {
                content: msg.content.clone(),
                file_paths: vec![],
            }
        } else {
            // Need to reprocess media (download + transcribe).
            let p = self
                .process_message(&msg.trigger_msg_id, &msg.msg_type, &msg.content)
                .await;
            // Update DB to ready.
            if let Err(e) = self
                .db
                .messages
                .save_ready(msg.db_msg_id, &p.content, &p.file_paths)
                .await
            {
                warn!(db_msg_id = msg.db_msg_id, error = %e, "recovery save_ready failed");
            }
            p
        };

        // Submit message to agent (same as normal flow).
        let (ready_tx, ready_rx) = oneshot::channel();
        let agent_msg = Message {
            msg_id: msg.trigger_msg_id.clone(),
            channel: msg.channel,
            db_msg_id: Some(msg.db_msg_id),
            ready: ready_rx,
            port: self.tx.clone(),
        };
        if let Err(e) = self.task_tx.send(agent_msg).await {
            warn!(db_msg_id = msg.db_msg_id, error = %e, "recovery submit failed");
            return;
        }
        let _ = ready_tx.send(payload);
        debug!(db_msg_id = msg.db_msg_id, "recovery message submitted");
    }

    /// Slow path: download media, transcribe audio, send ready signal.
    async fn process_media(&self, work: MediaWork) {
        let payload = self
            .process_message(&work.msg_id, &work.msg_type, &work.raw_content)
            .await;
        debug!(
            msg_id = work.msg_id,
            content_len = payload.content.len(),
            files = payload.file_paths.len(),
            "payload ready"
        );
        if let Some(db_id) = work.db_msg_id
            && let Err(e) = self
                .db
                .messages
                .save_ready(db_id, &payload.content, &payload.file_paths)
                .await
        {
            warn!(msg_id = work.msg_id, error = %e, "save_ready failed");
        }
        let _ = work.ready_tx.send(payload);
    }

    /// Fast path: parse, dedup, persist, submit task. Returns MediaWork
    /// for the slow path (media download + transcription).
    async fn handle_inbound(&self, event: feishu::types::FeishuEvent) -> Option<MediaWork> {
        let feishu::types::FeishuEvent::Message(envelope) = event else {
            debug!("ignoring non-message event");
            return None;
        };

        let header = match &envelope.header {
            Some(h) => h,
            None => return None,
        };
        if header.event_type.as_deref() != Some("im.message.receive_v1") {
            debug!(
                event_type = header.event_type.as_deref().unwrap_or(""),
                "ignoring non-IM event"
            );
            return None;
        }

        let event_data = match &envelope.event {
            Some(data) => data,
            None => return None,
        };
        let message = match event_data.get("message") {
            Some(m) => m,
            None => {
                warn!("inbound event missing 'message' field");
                return None;
            }
        };

        let msg_id = message
            .get("message_id")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        let chat_type = message
            .get("chat_type")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        let chat_id = if chat_type == "p2p" {
            event_data
                .get("sender")
                .and_then(|s| s.get("sender_id"))
                .and_then(|s| s.get("open_id"))
                .and_then(|v| v.as_str())
                .unwrap_or("")
        } else {
            message
                .get("chat_id")
                .and_then(|v| v.as_str())
                .unwrap_or("")
        };
        if msg_id.is_empty() || chat_id.is_empty() {
            warn!(msg_id, chat_id, "inbound event missing msg_id or chat_id");
            return None;
        }

        // Dedup by message_id. Feishu WS may deliver the same event multiple times.
        {
            let mut seen = self.seen_msg_ids.lock().unwrap();
            if seen.len() > 10_000 {
                seen.clear();
            }
            if !seen.insert(msg_id.to_string()) {
                debug!(msg_id, "duplicate message, skipping");
                return None;
            }
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
        let source_ts = envelope
            .header
            .as_ref()
            .and_then(|h| h.create_time.as_deref())
            .and_then(|t| t.parse::<i64>().ok())
            .and_then(chrono::DateTime::from_timestamp_millis);
        let channel = if chat_type == "p2p" {
            format!("feishu:p2p:{chat_id}")
        } else {
            format!("feishu:group:{chat_id}")
        };
        let db_msg_id = match self
            .db
            .messages
            .save_received(&InboundMessage {
                channel: &channel,
                content: &raw_content,
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

        let agent_msg = Message {
            msg_id: msg_id.clone(),
            channel: channel.clone(),
            db_msg_id,
            ready: ready_rx,
            port: self.tx.clone(),
        };
        if let Err(e) = self.task_tx.send(agent_msg).await {
            warn!(msg_id, chat_id, error = %e, "submit message failed");
            return None;
        }
        debug!(msg_id, chat_id, "message submitted to agent");

        // Return media work for the select loop to drive concurrently.
        Some(MediaWork {
            msg_id,
            msg_type,
            raw_content,
            db_msg_id,
            ready_tx,
        })
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
        let Some(file_key) = file_key else {
            return Payload {
                content: "The user sent a voice message.".into(),
                file_paths: vec![],
            };
        };

        let filename = match self
            .download_and_save(msg_id, file_key, "file", ".opus")
            .await
        {
            Ok(f) => f,
            Err(e) => {
                warn!(msg_id, file_key, error = %e, "audio download failed");
                return Payload {
                    content: "The user sent a voice message (download failed).".into(),
                    file_paths: vec![],
                };
            }
        };

        // Transcribe if client is configured.
        let transcript = if let Some(tc) = &self.transcriber {
            let abs_path = self.workspace_dir.join(MEDIA_DIR).join(&filename);
            match tokio::fs::read(&abs_path).await {
                Ok(bytes) => match tc.transcribe(&bytes, &filename).await {
                    Ok(result) => {
                        info!(msg_id, confidence = result.confidence, "audio transcribed");
                        result.text
                    }
                    Err(e) => {
                        warn!(msg_id, error = %e, "transcription failed");
                        "The user sent a voice message (transcription failed).".into()
                    }
                },
                Err(e) => {
                    warn!(msg_id, error = %e, "read audio file failed");
                    "The user sent a voice message (transcription failed).".into()
                }
            }
        } else {
            "The user sent a voice message (transcription not configured).".into()
        };

        Payload {
            content: transcript,
            file_paths: vec![filename],
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
            Ok(filename) => {
                crate::embedder::process_upload(self.db, &self.workspace_dir, file_name, &filename)
                    .await;
                Payload {
                    content: format!("The user sent a file '{file_name}'."),
                    file_paths: vec![filename],
                }
            }
            Err(e) => {
                warn!(msg_id, file_key, file_name, error = %e, "file download failed");
                Payload {
                    content: format!("The user sent a file '{file_name}' (download failed)."),
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
    async fn download_and_save(
        &self,
        msg_id: &str,
        file_key: &str,
        resource_type: &str,
        ext: &str,
    ) -> Result<String, feishu::types::Error> {
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

        info!(
            file_key,
            filename,
            size = data.len(),
            "media file downloaded"
        );
        Ok(filename)
    }

    /// Render a single notification: open thinking card → consume events → update card.
    /// Takes ownership of `notif` so it can live inside FuturesUnordered.
    async fn render_one(&self, api: &feishu::api::Api, mut notif: Notification) {
        let lang = "zh";

        let card_id = match api
            .reply_message(&notif.msg_id, "interactive", &thinking_card(lang))
            .await
        {
            Ok(id) => {
                debug!(msg_id = %notif.msg_id, card_id = %id, "thinking card opened");
                Some(id)
            }
            Err(e) => {
                warn!(msg_id = %notif.msg_id, error = %e, "failed to open thinking card");
                None
            }
        };

        let mut got_reply = false;
        // (tool_name, display_text) — ordered by first appearance, deduped by name.
        let mut status_lines: Vec<(String, String)> = Vec::new();

        while let Some(event) = notif.events.recv().await {
            match event {
                Event::ToolStatus { tool, text } => {
                    // Upsert: same tool name updates its line, new tool appends.
                    if let Some(entry) = status_lines.iter_mut().find(|(k, _)| k == &tool) {
                        entry.1 = text;
                    } else {
                        status_lines.push((tool, text));
                    }
                    if let Some(cid) = &card_id {
                        let card = build_status_card(&status_lines, false);
                        let _ = api.update_message(cid, &card).await;
                    }
                }
                Event::Composing => {
                    if let Some(cid) = &card_id {
                        let card = build_status_card(&status_lines, true);
                        let _ = api.update_message(cid, &card).await;
                    }
                }
                Event::Reply { text } => {
                    got_reply = true;
                    let processed = crate::render::process_markdown(&text, api).await;
                    let card_json = crate::render::markdown_to_card(&processed);
                    if let Some(cid) = &card_id {
                        match api.update_message(cid, &card_json).await {
                            Ok(()) => {
                                info!(msg_id = %notif.msg_id, "reply updated on card");
                            }
                            Err(e) => {
                                warn!(msg_id = %notif.msg_id, error = %e, "card update failed, sending new reply");
                                let content = serde_json::json!({"text": text}).to_string();
                                let _ = api.reply_message(&notif.msg_id, "text", &content).await;
                            }
                        }
                    } else {
                        let content = serde_json::json!({"text": text}).to_string();
                        if let Err(e) = api.reply_message(&notif.msg_id, "text", &content).await {
                            warn!(msg_id = %notif.msg_id, error = %e, "reply failed");
                        }
                    }
                }
            }
        }

        if !got_reply && let Some(cid) = &card_id {
            let _ = api
                .update_message(cid, &crate::render::markdown_to_card("✓"))
                .await;
        }

        debug!(msg_id = %notif.msg_id, "notification rendering complete");
    }
}

#[async_trait::async_trait]
impl super::Im for Feishu<'_> {
    async fn run(&self, cancel: &CancellationToken) {
        Feishu::run(self, cancel).await;
    }
}

// --- Feishu card builders ---

/// Build a card showing stacked tool status lines.
/// Replaces the initial "thinking" card on first call.
fn build_status_card(lines: &[(String, String)], composing: bool) -> String {
    let mut all: Vec<&str> = lines.iter().map(|(_, text)| text.as_str()).collect();
    if composing {
        all.push("✏️ Composing answer...");
    }
    crate::render::markdown_to_card(&all.join("\n"))
}

fn thinking_card(lang: &str) -> String {
    let text = if lang == "zh" {
        "💭 正在思考..."
    } else {
        "💭 Thinking..."
    };
    crate::render::markdown_to_card(text)
}

// --- Content extraction helpers ---

fn extract_text(raw: &str) -> String {
    serde_json::from_str::<serde_json::Value>(raw)
        .ok()
        .and_then(|v| v.get("text").and_then(|t| t.as_str()).map(String::from))
        .unwrap_or_else(|| raw.to_string())
}

fn extract_post_content(parsed: &serde_json::Value) -> (String, Vec<String>) {
    if let Some(content) = parsed.get("content") {
        let title = parsed.get("title").and_then(|t| t.as_str()).unwrap_or("");
        let (body, images) = parse_post_content_array(content);
        return (join_text(title, &body), images);
    }

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
