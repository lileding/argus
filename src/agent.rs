use std::sync::Arc;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::database::Database;
use crate::server::Server;
use crate::upstream;
use crate::upstream::types as model;

// --- Types ---

pub struct Payload {
    pub(crate) content: String,
    pub file_paths: Vec<String>,
}

/// Trait for receiving messages from the Agent. Defined in the agent
/// module so Agent never imports frontend. Frontend implements this.
#[async_trait::async_trait]
pub trait MessageSink: Send + Sync {
    async fn submit_message(&self, msg: Message);
}

pub(crate) struct Task {
    pub(crate) chat_id: String,
    pub(crate) msg_id: String,
    /// Database row ID (None if DB not available).
    pub(crate) db_msg_id: Option<i64>,
    pub(crate) ready: oneshot::Receiver<Payload>,
    pub(crate) frontend: Arc<dyn MessageSink>,
}

pub(crate) struct Message {
    #[allow(dead_code)] // Will be used for per-chat channel routing.
    pub(crate) chat_id: String,
    pub msg_id: String,
    /// Agent emits events; frontend consumes them to drive UI.
    /// Dropping the sender signals "message complete".
    pub events: mpsc::Receiver<Event>,
}

pub enum Event {
    Reply { text: String },
}

// --- Prompts ---

/// Orchestrator prompt (Phase 1). When tools are available, this will instruct
/// the model to only call tools. For now (no tools), it analyzes the request.
const ORCHESTRATOR_PROMPT: &str = r#"You are the ORCHESTRATOR of an AI agent. Analyze the user's request and produce a structured summary:

1. Identify the user's intent and key topics
2. Note any specific facts, entities, or constraints mentioned
3. Determine what information would be needed to answer well

Output a brief, structured analysis. The SYNTHESIZER will use your analysis to compose the final answer."#;

/// Synthesizer prompt (Phase 2). Composes the final user-facing answer.
const SYNTHESIZER_PROMPT: &str = r#"You are the SYNTHESIZER. You receive the user's question and an analysis from the orchestrator. Compose a clear, helpful answer.

RULES:
- Match the user's language and tone. If they asked in Chinese, answer in Chinese.
- Use markdown formatting: headings, lists, code blocks.
- Be concise, well-structured, and directly address the user's question.
- Draw on your knowledge to provide a thorough answer."#;

// --- Agent ---

#[derive(Debug, thiserror::Error)]
pub enum AgentError {
    #[error("task queue closed")]
    QueueClosed,
}

impl<T> From<mpsc::error::SendError<T>> for AgentError {
    fn from(_: mpsc::error::SendError<T>) -> Self {
        Self::QueueClosed
    }
}

pub struct Agent {
    // Note: Agent holds tx, so rx.recv() never returns None due to "all senders dropped".
    // The run loop exits via CancellationToken, not channel close.
    tx: mpsc::Sender<Task>,
    rx: Mutex<mpsc::Receiver<Task>>,
    cancel: CancellationToken,
    db: Arc<Database>,
    orchestrator: Arc<dyn upstream::Client>,
    synthesizer: Arc<dyn upstream::Client>,
}

impl Agent {
    pub(crate) fn new(
        config: &crate::config::AgentConfig,
        upstream: &upstream::Upstream,
        db: &Arc<Database>,
    ) -> Result<Arc<Self>, upstream::types::ClientError> {
        let orchestrator = upstream.client_for(&config.orchestrator)?;
        let synthesizer = upstream.client_for(&config.synthesizer)?;

        info!(
            orchestrator = config.orchestrator.model_name,
            synthesizer = config.synthesizer.model_name,
            "agent initialized"
        );

        let (tx, rx) = mpsc::channel(64);
        Ok(Arc::new(Agent {
            tx,
            rx: Mutex::new(rx),
            cancel: CancellationToken::new(),
            db: Arc::clone(db),
            orchestrator,
            synthesizer,
        }))
    }

    async fn run_inner(&self) {
        info!("agent scheduler started");
        let mut rx = self.rx.lock().await;
        loop {
            tokio::select! {
                task = rx.recv() => {
                    let Some(task) = task else {
                        debug!("agent task channel closed");
                        break;
                    };
                    self.process_task(task).await;
                }
                _ = self.cancel.cancelled() => {
                    info!("agent received shutdown signal");
                    break;
                }
            }
        }
        info!("agent scheduler stopped");
    }

    /// Two-phase processing pipeline:
    /// 1. Orchestrator: analyze user message (no tools for now → just acknowledges)
    /// 2. Synthesizer: compose final answer from materials, streaming to frontend
    async fn process_task(&self, task: Task) {
        let chat_id = &task.chat_id;
        let msg_id = &task.msg_id;
        info!(chat_id, msg_id, "processing task");

        // Open events channel → frontend shows thinking card.
        let (events_tx, events_rx) = mpsc::channel(16);
        let msg = Message {
            chat_id: task.chat_id.clone(),
            msg_id: task.msg_id.clone(),
            events: events_rx,
        };
        task.frontend.submit_message(msg).await;

        // Wait for payload.
        let payload = match task.ready.await {
            Ok(p) => p,
            Err(_) => {
                warn!(chat_id, msg_id, "ready channel dropped");
                return;
            }
        };
        debug!(
            chat_id,
            msg_id,
            content_len = payload.content.len(),
            "payload received"
        );

        // Phase 1: Orchestrator — no tools available yet, so it just produces
        // a summary/analysis. With tools, this would be a loop.
        let orch_summary = match self.run_orchestrator(&payload.content).await {
            Ok(summary) => {
                debug!(
                    chat_id,
                    msg_id,
                    summary_len = summary.len(),
                    "orchestrator done"
                );
                summary
            }
            Err(e) => {
                warn!(chat_id, msg_id, error = %e, "orchestrator failed");
                // Fallback: empty materials, synthesizer works from user text alone.
                String::new()
            }
        };

        // Phase 2: Synthesizer — stream the final answer to frontend.
        let reply_text = match self
            .run_synthesizer(&payload.content, &orch_summary, &events_tx)
            .await
        {
            Ok(text) => {
                debug!(chat_id, msg_id, "synthesizer done");
                text
            }
            Err(e) => {
                warn!(chat_id, msg_id, error = %e, "synthesizer failed");
                let error_text = format!("Error: {e}");
                let _ = events_tx
                    .send(Event::Reply {
                        text: error_text.clone(),
                    })
                    .await;
                error_text
            }
        };

        // Persist reply and mark as terminal.
        if let Some(db_id) = task.db_msg_id
            && let Err(e) = self.db.messages.save_replied(db_id, &reply_text).await
        {
            warn!(chat_id, msg_id, error = %e, "save_replied failed");
        }

        info!(chat_id, msg_id, "task complete");
    }

    /// Phase 1: Call the orchestrator model.
    /// Without tools, it just analyzes the user's message and produces a summary.
    async fn run_orchestrator(
        &self,
        user_text: &str,
    ) -> Result<String, upstream::types::ClientError> {
        let messages = vec![
            model::Message::system(ORCHESTRATOR_PROMPT),
            model::Message::user(user_text),
        ];

        // No tools → simple chat call. The orchestrator will produce text
        // (which we use as "materials" for the synthesizer).
        let response = self.orchestrator.chat(&messages, &[]).await?;
        Ok(response.content)
    }

    /// Phase 2: Call the synthesizer model with streaming.
    /// Returns the full reply text.
    async fn run_synthesizer(
        &self,
        user_text: &str,
        materials: &str,
        events_tx: &mpsc::Sender<Event>,
    ) -> Result<String, upstream::types::ClientError> {
        use futures::StreamExt;

        // Single user message with both the question and materials to avoid
        // consecutive same-role messages (which some providers reject).
        let user_content = format!(
            "{}\n\n---\n\n## Orchestrator Analysis\n\n{}",
            user_text, materials
        );
        let messages = vec![
            model::Message::system(SYNTHESIZER_PROMPT),
            model::Message::user(user_content),
        ];

        let mut stream = self.synthesizer.chat_stream(&messages, &[]).await?;
        let mut full_reply = String::new();

        while let Some(chunk) = stream.next().await {
            if let Some(err) = &chunk.error {
                return Err(upstream::types::ClientError::Sse(err.clone()));
            }
            if !chunk.delta.is_empty() {
                full_reply.push_str(&chunk.delta);
            }
            if chunk.done {
                break;
            }
        }

        // Send the complete reply as a single event.
        // (Future: send incremental deltas for streaming card updates.)
        let _ = events_tx
            .send(Event::Reply {
                text: full_reply.clone(),
            })
            .await;

        Ok(full_reply)
    }

    pub async fn submit_task(&self, task: Task) -> Result<(), AgentError> {
        self.tx.send(task).await?;
        Ok(())
    }
}

#[async_trait::async_trait]
impl Server for Agent {
    async fn run(self: Arc<Self>) {
        self.run_inner().await;
    }

    async fn stop(&self) {
        self.cancel.cancel();
    }
}
