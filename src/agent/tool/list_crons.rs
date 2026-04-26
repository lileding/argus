use serde_json::Value;

use crate::database::Database;

use super::{Tool, ToolContext};

pub(super) struct ListCrons<'a> {
    db: &'a Database,
}

impl<'a> ListCrons<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }
}

#[async_trait::async_trait]
impl Tool for ListCrons<'_> {
    fn name(&self) -> &str {
        "list_crons"
    }

    fn description(&self) -> &str {
        "List all active scheduled tasks (crons) for this chat. \
         Use this when the user asks 'what reminders do I have' or similar."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {}
        })
    }

    async fn execute(&self, ctx: &ToolContext<'_>, _args: &str) -> String {
        match self.db.crons.list_for_channel(ctx.channel).await {
            Ok(crons) if crons.is_empty() => "No active scheduled tasks.".to_string(),
            Ok(crons) => {
                let mut out = String::new();
                for c in crons {
                    let last = c
                        .last_run_at
                        .map(|t| t.to_rfc3339())
                        .unwrap_or_else(|| "never".into());
                    out.push_str(&format!(
                        "Cron #{id} | {expr} | last_run: {last}\n  goal: {goal}\n",
                        id = c.id,
                        expr = c.cron_expr,
                        goal = super::truncate_display(&c.goal, 200)
                    ));
                }
                out
            }
            Err(e) => format!("error: {e}"),
        }
    }

    fn status_label(&self, _args: &str) -> String {
        "📋 Listing scheduled tasks".into()
    }
}
