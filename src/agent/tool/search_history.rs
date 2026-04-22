use serde::Deserialize;
use serde_json::Value;

use crate::database::Database;

use super::super::EmbedService;
use super::Tool;

pub(super) struct SearchHistory<'a, E: EmbedService> {
    db: &'a Database,
    embed_service: &'a E,
}

#[derive(Deserialize)]
struct Args {
    query: String,
    limit: Option<i64>,
}

impl<'a, E: EmbedService> SearchHistory<'a, E> {
    pub(super) fn new(db: &'a Database, embed_service: &'a E) -> Self {
        Self { db, embed_service }
    }
}

#[async_trait::async_trait]
impl<'a, E: EmbedService> Tool for SearchHistory<'a, E> {
    fn name(&self) -> &str {
        "search_history"
    }

    fn description(&self) -> &str {
        "Search past conversation history using semantic similarity. Use this to recall previous \
         discussions, answers, or context from earlier conversations."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "query": {
                    "type": "string",
                    "description": "The search query."
                },
                "limit": {
                    "type": "integer",
                    "description": "Maximum number of results (default 5)."
                }
            },
            "required": ["query"]
        })
    }

    async fn execute(&self, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let limit = parsed.limit.unwrap_or(5);

        let embedding = match self.embed_service.embed_one(&parsed.query).await {
            Ok(v) => v,
            Err(e) => return format!("error: embedding failed: {e}"),
        };

        // Search both user messages and agent replies across all channels.
        let mut output = String::new();
        let mut idx = 0;

        // Search user messages across all channels.
        if let Ok(results) = self.db.conversation.search_all(&embedding, limit).await {
            for (_, similarity, user_msg, reply_msg) in &results {
                idx += 1;
                output.push_str(&format!(
                    "{}. [user, similarity: {:.2}]\n{}\n",
                    idx, similarity, user_msg.content,
                ));
                if let Some(reply) = reply_msg {
                    output.push_str(&format!("   → reply: {}\n", reply.content));
                }
                output.push('\n');
            }
        }

        // Also search agent replies (notifications).
        if let Ok(results) = self.db.conversation.search_replies(&embedding, limit).await {
            for (similarity, msg) in &results {
                idx += 1;
                output.push_str(&format!(
                    "{}. [reply, similarity: {:.2}]\n{}\n\n",
                    idx, similarity, msg.content,
                ));
            }
        }

        if idx == 0 {
            return "No matching conversation history found.".into();
        }
        output
    }

    fn status_label(&self, args: &str) -> String {
        let query = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("query")?.as_str().map(String::from))
            .unwrap_or_default();
        if query.is_empty() {
            return "🔎 Searching history".into();
        }
        format!(
            "🔎 Searching history: {}",
            super::truncate_display(&query, 40)
        )
    }
}
