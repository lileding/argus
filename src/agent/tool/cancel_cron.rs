use serde::Deserialize;
use serde_json::Value;

use crate::database::Database;

use super::{Tool, ToolContext};

pub(super) struct CancelCron<'a> {
    db: &'a Database,
}

#[derive(Deserialize)]
struct Args {
    id: i64,
}

impl<'a> CancelCron<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }
}

#[async_trait::async_trait]
impl Tool for CancelCron<'_> {
    fn name(&self) -> &str {
        "cancel_cron"
    }

    fn description(&self) -> &str {
        "Cancel a scheduled task by its numeric ID (e.g. cancel #4). \
         Use this when the user says 'stop the daily summary', 'cancel task 4', etc."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "id": {
                    "type": "integer",
                    "description": "The numeric ID of the cron to cancel."
                }
            },
            "required": ["id"]
        })
    }

    async fn execute(&self, ctx: &ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };
        match self.db.crons.cancel(parsed.id, ctx.channel).await {
            Ok(true) => format!("Cron #{} cancelled.", parsed.id),
            Ok(false) => format!("error: Cron #{} not found in this chat", parsed.id),
            Err(e) => format!("error: {e}"),
        }
    }

    fn status_label(&self, args: &str) -> String {
        let id = serde_json::from_str::<Args>(args)
            .map(|a| a.id.to_string())
            .unwrap_or_default();
        format!("🚫 Cancelling Cron #{id}")
    }
}
