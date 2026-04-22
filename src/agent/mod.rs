mod embedding;
mod harness;
mod worker;

use std::sync::Arc;
use std::time::Duration;

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
    /// Database channel (e.g. "feishu:p2p:ou_xxx").
    pub(crate) channel: String,
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

pub(crate) struct Agent {
    tx: mpsc::Sender<Task>,
    rx: Mutex<mpsc::Receiver<Task>>,
    cancel: CancellationToken,
    db: Arc<Database>,
    orchestrator: Arc<dyn upstream::Client>,
    synthesizer: Arc<dyn upstream::Client>,
    embedder: Option<Arc<embedding::EmbeddingClient>>,
    embedding_interval: Duration,
    embedding_batch_size: usize,
}

impl Agent {
    pub(crate) fn new(
        config: &crate::config::AgentConfig,
        upstream_reg: &upstream::Upstream,
        db: &Arc<Database>,
    ) -> Result<Arc<Self>, upstream::types::ClientError> {
        let orchestrator = upstream_reg.client_for(&config.orchestrator)?;
        let synthesizer = upstream_reg.client_for(&config.synthesizer)?;

        // Embedding client (optional: only if upstream is configured).
        let embedder = if !config.embedding.upstream.is_empty() {
            let up_cfg = upstream_reg
                .get_config(&config.embedding.upstream)
                .ok_or_else(|| {
                    upstream::types::ClientError::Other(format!(
                        "embedding upstream '{}' not found",
                        config.embedding.upstream
                    ))
                })?;
            let base_url = up_cfg.effective_base_url();
            Some(Arc::new(embedding::EmbeddingClient::new(
                base_url,
                &up_cfg.api_key,
                &config.embedding.model_name,
            )))
        } else {
            None
        };

        info!(
            orchestrator = config.orchestrator.model_name,
            synthesizer = config.synthesizer.model_name,
            embedding = embedder.is_some(),
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
            embedder,
            embedding_interval: Duration::from_secs(config.embedding.interval_secs),
            embedding_batch_size: config.embedding.batch_size,
        }))
    }

    async fn run_inner(self: &Arc<Self>) {
        info!("agent scheduler started");

        // Spawn background workers.
        let mut worker_handles = Vec::new();
        if let Some(embedder) = &self.embedder {
            worker_handles.push(worker::spawn_embedder(
                Arc::clone(&self.db),
                Arc::clone(embedder),
                self.cancel.clone(),
                self.embedding_batch_size,
                self.embedding_interval,
            ));
            worker_handles.push(worker::spawn_summarizer(
                Arc::clone(&self.db),
                Arc::clone(&self.synthesizer),
                self.cancel.clone(),
                self.embedding_interval * 5, // summarize less frequently
            ));
        }

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
        // Wait for background workers to finish.
        for handle in worker_handles {
            let _ = handle.await;
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

        // Build context with conversation history (semantic recall + sliding window).
        let orch_messages = harness::build_context(
            &self.db,
            self.embedder.as_deref(),
            ORCHESTRATOR_PROMPT,
            &task.channel,
            &payload.content,
            task.db_msg_id, // exclude current message from history
            10,             // context window
        )
        .await;

        // Phase 1: Orchestrator — calls model with full context.
        let orch_summary = match self.orchestrator.chat(&orch_messages, &[]).await {
            Ok(resp) => {
                debug!(
                    chat_id,
                    msg_id,
                    summary_len = resp.content.len(),
                    "orchestrator done"
                );
                resp.content
            }
            Err(e) => {
                warn!(chat_id, msg_id, error = %e, "orchestrator failed");
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

        // Persist reply as notification + link to message.
        if let Err(e) = self
            .db
            .notifications
            .save_notification(task.db_msg_id, &reply_text)
            .await
        {
            warn!(chat_id, msg_id, error = %e, "save_notification failed");
        }

        info!(chat_id, msg_id, "task complete");
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
