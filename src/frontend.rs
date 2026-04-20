use std::sync::Arc;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::agent::{Agent, Event, Message, Payload, Task};

pub struct Feishu {
    agent: Arc<Agent>,
    cancel: CancellationToken,
    /// Outbound channel: Agent sends Messages here, render loop consumes.
    tx: mpsc::Sender<Message>,
    rx: Mutex<mpsc::Receiver<Message>>,
    feishu_client: feishu::Client,
}

impl Feishu {
    pub fn new(agent: Arc<Agent>, app_id: &str, app_secret: &str) -> Arc<Self> {
        let (tx, rx) = mpsc::channel(64);
        Arc::new(Feishu {
            agent,
            cancel: CancellationToken::new(),
            tx,
            rx: Mutex::new(rx),
            feishu_client: feishu::Client::new(app_id, app_secret),
        })
    }

    /// Main frontend loop: connects WS, dispatches inbound events,
    /// reconnects on disconnect. Outbound rendering runs in a spawned task.
    pub async fn run(self: &Arc<Self>) {
        info!("feishu frontend started");

        // Spawn outbound render loop (consumes Messages from Agent).
        let outbound_handle = {
            let this = Arc::clone(self);
            tokio::spawn(async move { this.handle_outbound().await })
        };

        // Inbound: connect WS and receive events, reconnect on disconnect.
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

            // Per-connection event loop.
            loop {
                tokio::select! {
                    event = ws.next_event() => {
                        let Some(event) = event else {
                            info!("feishu WS disconnected, will reconnect");
                            break; // break inner loop → reconnect
                        };
                        let this = Arc::clone(self);
                        // Spawn inbound handling so we don't block the WS recv loop.
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
    /// ReadyCh, submit to Agent, and spawn a trivial "download" that extracts
    /// text content and delivers it through the oneshot.
    async fn handle_inbound(self: Arc<Self>, event: feishu::types::FeishuEvent) {
        // Only handle message events for now; ignore card callbacks.
        let feishu::types::FeishuEvent::Message(envelope) = event else {
            debug!("ignoring non-message event");
            return;
        };

        let header = match &envelope.header {
            Some(h) => h,
            None => return,
        };
        let event_type = header.event_type.as_deref().unwrap_or("");
        if event_type != "im.message.receive_v1" {
            debug!(event_type, "ignoring non-IM event");
            return;
        }

        let event_data = match &envelope.event {
            Some(data) => data,
            None => return,
        };

        // Extract fields from the nested Feishu event JSON.
        let msg_id = event_data
            .get("message")
            .and_then(|m| m.get("message_id"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();

        let chat_id = event_data
            .get("message")
            .and_then(|m| m.get("chat_id"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();

        let raw_content = event_data
            .get("message")
            .and_then(|m| m.get("content"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();

        info!(msg_id, chat_id, "inbound message received");

        // ReadyCh: oneshot that carries the processed Payload.
        let (ready_tx, ready_rx) = oneshot::channel();

        // Submit task to Agent. The Agent will:
        // 1. Send a Message to our outbound channel (opens card)
        // 2. Await this oneshot for the payload
        // 3. Process and emit events
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

        // Spawn trivial "download": extract text from Feishu content JSON.
        // In production this would download media, transcribe audio, etc.
        tokio::spawn(async move {
            let text = extract_text(&raw_content);
            debug!(msg_id, chat_id, content_len = text.len(), "payload ready");
            let _ = ready_tx.send(Payload { content: text });
        });
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

                    // Consume all events from the Agent for this message.
                    while let Some(event) = msg.events.recv().await {
                        match event {
                            Event::Reply { text } => {
                                debug!(msg_id = %msg.msg_id, reply_len = text.len(), "sending reply");
                                let content = serde_json::json!({"text": text}).to_string();
                                match api.reply_message(&msg.msg_id, "text", &content).await {
                                    Ok(reply_id) => {
                                        info!(
                                            msg_id = %msg.msg_id,
                                            reply_id,
                                            "reply sent"
                                        );
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
                    info!("outbound render loop shutting down");
                    break;
                }
            }
        }
        info!("outbound render loop stopped");
    }
}

/// Extract text from Feishu message content JSON.
/// Text messages have content like `{"text":"hello @_user_1"}`.
fn extract_text(raw: &str) -> String {
    serde_json::from_str::<serde_json::Value>(raw)
        .ok()
        .and_then(|v| v.get("text").and_then(|t| t.as_str()).map(String::from))
        .unwrap_or_else(|| raw.to_string())
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
}
