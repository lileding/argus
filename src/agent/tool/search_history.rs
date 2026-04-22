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
        "Search past conversation history using semantic similarity. Use SHORT, SPECIFIC keywords \
         as the query — not full sentences. For example, to find when the user asked about \
         Schrödinger's equation, search \"薛定谔方程\" not \"第一次问薛定谔方程是什么时候\". \
         Results include timestamps."
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

    async fn execute(&self, ctx: &super::ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let limit = parsed.limit.unwrap_or(5);

        let embedding = match self.embed_service.embed_one(&parsed.query).await {
            Ok(v) => v,
            Err(e) => return format!("error: embedding failed: {e}"),
        };

        // Search user messages + replies with timestamps.
        let results = match self
            .db
            .conversation
            .search_with_time(&embedding, ctx.channel, limit)
            .await
        {
            Ok(r) => r,
            Err(e) => return format!("error: {e}"),
        };

        if results.is_empty() {
            return "No matching conversation history found.".into();
        }

        let mut output = String::new();
        for (i, (similarity, ts, user_content, reply_content)) in results.iter().enumerate() {
            let time = ts.format("%Y-%m-%d %H:%M");
            output.push_str(&format!(
                "{}. [{time}, similarity: {:.2}]\nUser: {}\n",
                i + 1,
                similarity,
                user_content,
            ));
            if let Some(reply) = reply_content {
                output.push_str(&format!("Reply: {reply}\n"));
            }
            output.push('\n');
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
