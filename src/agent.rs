use std::sync::Arc;
use tokio::sync::{Mutex, mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, warn};

// --- Types ---

pub struct Payload {
    pub content: String,
}

pub struct Task {
    pub chat_id: String,
    pub msg_id: String,
    /// Delivers the processed payload once media download/extraction is done.
    pub ready: oneshot::Receiver<Payload>,
    /// Channel back to the frontend for outbound rendering.
    pub frontend: mpsc::Sender<Message>,
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

    /// Main agent loop: pops tasks from the queue, processes them serially.
    pub async fn run(&self) {
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
        if task.frontend.send(msg).await.is_err() {
            warn!(chat_id, msg_id, "frontend channel closed, dropping task");
            return;
        }
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

    pub async fn stop(&self) {
        self.cancel.cancel();
    }

    pub async fn submit_task(&self, task: Task) -> Result<(), AgentError> {
        self.tx.send(task).await?;
        Ok(())
    }
}
