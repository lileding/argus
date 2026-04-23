use std::sync::atomic::{AtomicU32, Ordering};

use serde_json::Value;
use tokio::sync::mpsc;

use super::{Tool, ToolContext};
use crate::agent::TaskSpec;

pub(super) struct CreateTask<'a> {
    task_tx: &'a mpsc::Sender<TaskSpec>,
    next_id: &'a AtomicU32,
}

impl<'a> CreateTask<'a> {
    pub(super) fn new(task_tx: &'a mpsc::Sender<TaskSpec>, next_id: &'a AtomicU32) -> Self {
        Self { task_tx, next_id }
    }
}

#[async_trait::async_trait]
impl Tool for CreateTask<'_> {
    fn name(&self) -> &str {
        "create_task"
    }

    fn description(&self) -> &str {
        "Create an async background task for long-running work (research, \
         analysis, code generation). The task runs in the background and \
         sends a notification when complete. Use this when the user's \
         request requires extensive research or multi-step work that \
         would take too long for an immediate response."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "goal": {
                    "type": "string",
                    "description": "Detailed description of what the task should accomplish. \
                                    Be specific — this is the only instruction the background \
                                    worker receives."
                }
            },
            "required": ["goal"]
        })
    }

    async fn execute(&self, ctx: &ToolContext<'_>, args: &str) -> String {
        let goal = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("goal").and_then(|g| g.as_str()).map(String::from))
            .unwrap_or_default();

        if goal.is_empty() {
            return "error: 'goal' parameter is required".to_string();
        }

        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        let spec = TaskSpec {
            id,
            goal: goal.clone(),
            channel: ctx.channel.to_string(),
            msg_id: ctx.msg_id.to_string(),
            port: ctx.port.clone(),
        };

        if let Err(e) = self.task_tx.send(spec).await {
            return format!("error: failed to create task: {e}");
        }

        format!(
            "Created async task #{id}. The task will run in the background and send a notification when complete."
        )
    }

    fn status_label(&self, args: &str) -> String {
        let goal = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("goal").and_then(|g| g.as_str()).map(String::from))
            .unwrap_or_default();
        let preview = super::truncate_display(&goal, 40);
        format!("📋 Creating task: {preview}")
    }
}
