use std::sync::Arc;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

use crate::server::Server;

// --- Types ---

pub struct Payload {
    pub content: String,
    pub file_paths: Vec<String>,
}

/// Trait for receiving messages from the Agent. Defined in the agent
/// module so Agent never imports frontend. Frontend implements this.
#[async_trait::async_trait]
pub trait MessageSink: Send + Sync {
    async fn submit_message(&self, msg: Message);
}

pub struct Task {
    pub chat_id: String,
    pub msg_id: String,
    /// Delivers the processed payload once media download/extraction is done.
    pub ready: oneshot::Receiver<Payload>,
    /// Callback to the frontend for outbound rendering.
    pub frontend: Arc<dyn MessageSink>,
}

pub struct Message {
    pub chat_id: String,
    pub msg_id: String,
    /// Agent emits events; frontend consumes them to drive UI.
    /// Dropping the sender signals "message complete".
    pub events: mpsc::Receiver<Event>,
}

pub enum Event {
    Reply { text: String },
}

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
}

impl Agent {
    pub fn new() -> Arc<Self> {
        let (tx, rx) = mpsc::channel(64);
        Arc::new(Agent {
            tx,
            rx: Mutex::new(rx),
            cancel: CancellationToken::new(),
        })
    }

    /// Internal run logic, called by Server::run.
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

    /// Process a single task through the full pipeline:
    /// 1. Create events channel and notify frontend (opens card)
    /// 2. Wait for payload from media processor
    /// 3. Execute agent logic (echo for now)
    /// 4. Drop events channel (closes card)
    async fn process_task(&self, task: Task) {
        let chat_id = &task.chat_id;
        let msg_id = &task.msg_id;
        info!(chat_id, msg_id, "processing task");

        // Step 1: Create events channel and hand Message to frontend.
        // Frontend opens the thinking card as soon as it receives this.
        let (events_tx, events_rx) = mpsc::channel(16);
        let msg = Message {
            chat_id: task.chat_id.clone(),
            msg_id: task.msg_id.clone(),
            events: events_rx,
        };
        task.frontend.submit_message(msg).await;
        debug!(chat_id, msg_id, "message submitted to frontend");

        // Step 2: Wait for payload (media download/extraction complete).
        let payload = match task.ready.await {
            Ok(p) => {
                debug!(
                    chat_id,
                    msg_id,
                    content_len = p.content.len(),
                    "payload received from download"
                );
                p
            }
            Err(_) => {
                warn!(
                    chat_id,
                    msg_id, "ready channel dropped, no payload delivered"
                );
                return;
                // events_tx dropped here → frontend sees channel close → dismisses card
            }
        };

        // Step 3: Agent logic — echo for now.
        // Future: orchestrator tool loop → synthesizer streaming.
        let _ = events_tx
            .send(Event::Reply {
                text: payload.content,
            })
            .await;
        debug!(chat_id, msg_id, "reply event sent");

        // Step 4: events_tx dropped → frontend sees channel close → finalizes card.
        info!(chat_id, msg_id, "task complete");
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
