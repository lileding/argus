use std::str::FromStr;

use serde_json::Value;

use crate::database::Database;

use super::{Tool, ToolContext};

pub(super) struct CreateCron<'a> {
    db: &'a Database,
}

impl<'a> CreateCron<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }
}

#[async_trait::async_trait]
impl Tool for CreateCron<'_> {
    fn name(&self) -> &str {
        "create_cron"
    }

    fn description(&self) -> &str {
        "Create a recurring scheduled task. The task runs on the given schedule \
         and the result is sent back to the user. Use this when the user asks for \
         periodic reminders, summaries, or monitoring (e.g. 'every day at 6pm \
         summarize my new emails', 'remind me at 9pm if I haven't logged dinner'). \
         The execution prompt should be self-contained — the cron worker has no \
         memory of the conversation that created it. For conditional behavior \
         (e.g. only notify if X), include the condition check in the goal prompt."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "cron_expr": {
                    "type": "string",
                    "description": "Standard 6-field cron expression (sec min hour day month weekday). \
                                    System timezone. Examples: '0 0 18 * * *' = daily at 6pm; \
                                    '0 0 9 * * MON-FRI' = 9am on weekdays."
                },
                "goal": {
                    "type": "string",
                    "description": "The complete prompt to execute on each firing. Be self-contained \
                                    and explicit — the worker has no conversation context."
                }
            },
            "required": ["cron_expr", "goal"]
        })
    }

    async fn execute(&self, ctx: &ToolContext<'_>, args: &str) -> String {
        let v: Value = match serde_json::from_str(args) {
            Ok(v) => v,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };
        let cron_expr = v.get("cron_expr").and_then(|v| v.as_str()).unwrap_or("");
        let goal = v.get("goal").and_then(|v| v.as_str()).unwrap_or("");
        if cron_expr.is_empty() || goal.is_empty() {
            return "error: both 'cron_expr' and 'goal' are required".to_string();
        }
        // Validate cron expression.
        if let Err(e) = cron::Schedule::from_str(cron_expr) {
            return format!("error: invalid cron expression '{cron_expr}': {e}");
        }
        match self
            .db
            .crons
            .create(cron_expr, goal, ctx.channel, ctx.msg_id)
            .await
        {
            Ok(id) => format!(
                "Cron #{id} created. Schedule: {cron_expr}. Will run periodically and send results to this chat."
            ),
            Err(e) => format!("error: failed to create cron: {e}"),
        }
    }

    fn status_label(&self, _args: &str) -> String {
        "⏰ Creating scheduled task".into()
    }
}
