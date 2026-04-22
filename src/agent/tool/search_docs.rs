use serde::Deserialize;
use serde_json::Value;

use crate::database::Database;

use super::super::EmbedService;
use super::Tool;

pub(super) struct SearchDocs<'a, E: EmbedService> {
    db: &'a Database,
    embed_service: &'a E,
}

#[derive(Deserialize)]
struct Args {
    query: String,
    limit: Option<i64>,
    filename: Option<String>,
}

/// Max characters per chunk in output.
const MAX_CHUNK_DISPLAY: usize = 2000;

impl<'a, E: EmbedService> SearchDocs<'a, E> {
    pub(super) fn new(db: &'a Database, embed_service: &'a E) -> Self {
        Self { db, embed_service }
    }
}

#[async_trait::async_trait]
impl<'a, E: EmbedService> Tool for SearchDocs<'a, E> {
    fn name(&self) -> &str {
        "search_docs"
    }

    fn description(&self) -> &str {
        "Search through uploaded documents using semantic similarity. Use this to find \
         information in PDFs, notes, or other documents the user has uploaded."
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
                },
                "filename": {
                    "type": "string",
                    "description": "Optional filename filter (partial match)."
                }
            },
            "required": ["query"]
        })
    }

    async fn execute(&self, _ctx: &super::ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let limit = parsed.limit.unwrap_or(5);

        let embedding = match self.embed_service.embed_one(&parsed.query).await {
            Ok(v) => v,
            Err(e) => return format!("error: embedding failed: {e}"),
        };

        let results = match self
            .db
            .documents
            .search_chunks(&embedding, limit, parsed.filename.as_deref())
            .await
        {
            Ok(r) => r,
            Err(e) => return format!("error: {e}"),
        };

        if results.is_empty() {
            return "No matching documents found.".into();
        }

        let mut output = String::new();
        for (i, (filename, chunk_index, content, similarity)) in results.iter().enumerate() {
            let display_content = if content.chars().count() > MAX_CHUNK_DISPLAY {
                let truncated: String = content.chars().take(MAX_CHUNK_DISPLAY).collect();
                format!("{truncated} ...[truncated]")
            } else {
                content.clone()
            };
            output.push_str(&format!(
                "{}. [{}, chunk {}] (similarity: {:.2})\n{}\n\n",
                i + 1,
                filename,
                chunk_index,
                similarity,
                display_content,
            ));
        }
        output
    }

    fn status_label(&self, args: &str) -> String {
        let query = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("query")?.as_str().map(String::from))
            .unwrap_or_default();
        if query.is_empty() {
            return "📚 Searching docs".into();
        }
        format!("📚 Searching docs: {}", super::truncate_display(&query, 40))
    }
}
