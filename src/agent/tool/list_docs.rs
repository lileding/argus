use serde_json::Value;

use crate::database::Database;

use super::Tool;

pub(super) struct ListDocs<'a> {
    db: &'a Database,
}

impl<'a> ListDocs<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }
}

#[async_trait::async_trait]
impl<'a> Tool for ListDocs<'a> {
    fn name(&self) -> &str {
        "list_docs"
    }

    fn description(&self) -> &str {
        "List all uploaded documents with their status. Use this to see what documents are \
         available for searching."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {},
            "required": []
        })
    }

    async fn execute(&self, _ctx: &super::ToolContext<'_>, _args: &str) -> String {
        let docs = match self.db.documents.list_all().await {
            Ok(d) => d,
            Err(e) => return format!("error: {e}"),
        };

        if docs.is_empty() {
            return "No documents uploaded.".into();
        }

        let mut output = String::new();
        for (i, (id, filename, status, chunks, created_at)) in docs.iter().enumerate() {
            output.push_str(&format!(
                "{}. [id={}] {} — {} chunks, status: {}, uploaded: {}\n",
                i + 1,
                id,
                filename,
                chunks,
                status,
                created_at.format("%Y-%m-%d %H:%M"),
            ));
        }
        output
    }

    fn status_label(&self, _args: &str) -> String {
        "📋 Listing documents".into()
    }
}
