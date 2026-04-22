use serde::Deserialize;
use serde_json::Value;

use crate::database::Database;

use super::Tool;

pub(super) struct Forget<'a> {
    db: &'a Database,
}

#[derive(Deserialize)]
struct Args {
    id: String,
}

impl<'a> Forget<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }
}

#[async_trait::async_trait]
impl<'a> Tool for Forget<'a> {
    fn name(&self) -> &str {
        "forget"
    }

    fn description(&self) -> &str {
        "Remove a previously saved memory by its ID. Use this when the user asks you to forget \
         something or when a memory is no longer relevant."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "id": {
                    "type": "string",
                    "description": "The numeric ID of the memory to forget."
                }
            },
            "required": ["id"]
        })
    }

    async fn execute(&self, _ctx: &super::ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let id: i64 = match parsed.id.parse() {
            Ok(n) => n,
            Err(e) => return format!("error: invalid id \"{}\": {e}", parsed.id),
        };

        match self.db.memories.deactivate(id).await {
            Ok(true) => format!("Memory {id} forgotten."),
            Ok(false) => format!("error: memory {id} not found or already forgotten"),
            Err(e) => format!("error: {e}"),
        }
    }

    fn status_label(&self, _args: &str) -> String {
        "🗑️ Forgetting memory".into()
    }
}

#[cfg(test)]
mod tests {
    use super::Args;

    #[test]
    fn valid_numeric_id_parses() {
        let args: Args = serde_json::from_str(r#"{"id": "42"}"#).unwrap();
        let parsed: i64 = args.id.parse().unwrap();
        assert_eq!(parsed, 42);
    }

    #[test]
    fn non_numeric_id_fails_parse() {
        let args: Args = serde_json::from_str(r#"{"id": "abc"}"#).unwrap();
        let result = args.id.parse::<i64>();
        assert!(result.is_err(), "non-numeric id should fail i64 parse");
    }

    #[test]
    fn missing_id_fails() {
        let result = serde_json::from_str::<Args>("{}");
        assert!(result.is_err(), "missing 'id' should fail parsing");
    }
}
