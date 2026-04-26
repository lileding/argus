use std::str::FromStr;

use serde_json::Value;

use crate::database::Database;

use super::{Tool, ToolContext};

pub(super) struct UpdateCron<'a> {
    db: &'a Database,
}

impl<'a> UpdateCron<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }
}

#[async_trait::async_trait]
impl Tool for UpdateCron<'_> {
    fn name(&self) -> &str {
        "update_cron"
    }

    fn description(&self) -> &str {
        "Modify an existing scheduled task. Pass the ID and any fields to change \
         (cron_expr and/or goal). The cron's reply destination is updated to the \
         current message — future runs will reply here. Use when user says \
         'change task 4 to ...' or 'reschedule the daily summary to 7pm'."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "id": { "type": "integer", "description": "The cron ID to modify." },
                "cron_expr": {
                    "type": "string",
                    "description": "New 6-field cron expression (optional)."
                },
                "goal": {
                    "type": "string",
                    "description": "New execution prompt (optional)."
                }
            },
            "required": ["id"]
        })
    }

    async fn execute(&self, ctx: &ToolContext<'_>, args: &str) -> String {
        let v: Value = match serde_json::from_str(args) {
            Ok(v) => v,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };
        let id = match v.get("id").and_then(|v| v.as_i64()) {
            Some(id) => id,
            None => return "error: 'id' is required".to_string(),
        };
        let cron_expr = v.get("cron_expr").and_then(|v| v.as_str());
        let goal = v.get("goal").and_then(|v| v.as_str());
        if cron_expr.is_none() && goal.is_none() {
            return "error: provide cron_expr and/or goal to change".to_string();
        }
        if let Some(expr) = cron_expr
            && let Err(e) = cron::Schedule::from_str(expr)
        {
            return format!("error: invalid cron expression '{expr}': {e}");
        }
        match self
            .db
            .crons
            .update(id, ctx.channel, cron_expr, goal, ctx.msg_id)
            .await
        {
            Ok(true) => format!("Cron #{id} updated."),
            Ok(false) => format!("error: Cron #{id} not found in this chat"),
            Err(e) => format!("error: {e}"),
        }
    }

    fn status_label(&self, _args: &str) -> String {
        "✏️ Updating scheduled task".into()
    }
}
