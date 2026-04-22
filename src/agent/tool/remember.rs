use serde::Deserialize;
use serde_json::Value;

use crate::database::Database;

use super::Tool;

pub(super) struct Remember<'a> {
    db: &'a Database,
}

#[derive(Deserialize)]
struct Args {
    content: String,
    category: String,
}

impl<'a> Remember<'a> {
    pub(super) fn new(db: &'a Database) -> Self {
        Self { db }
    }
}

#[async_trait::async_trait]
impl<'a> Tool for Remember<'a> {
    fn name(&self) -> &str {
        "remember"
    }

    fn description(&self) -> &str {
        "Save a piece of information to long-term memory. Use this when the user tells you \
         something important about their preferences, facts, or instructions that should persist \
         across conversations."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "content": {
                    "type": "string",
                    "description": "The information to remember."
                },
                "category": {
                    "type": "string",
                    "enum": ["preference", "fact", "instruction"],
                    "description": "Category of memory: preference (user likes/dislikes), fact (information about user/world), instruction (how to behave)."
                }
            },
            "required": ["content", "category"]
        })
    }

    async fn execute(&self, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        // Validate category.
        match parsed.category.as_str() {
            "preference" | "fact" | "instruction" => {}
            other => {
                return format!(
                    "error: invalid category \"{other}\". Must be one of: preference, fact, instruction"
                );
            }
        }

        match self
            .db
            .memories
            .save(&parsed.category, &parsed.content)
            .await
        {
            Ok(id) => format!(
                "Remembered (id={id}, category={}): {}",
                parsed.category, parsed.content
            ),
            Err(e) => format!("error: {e}"),
        }
    }

    fn status_label(&self, _args: &str) -> String {
        "💾 Saving memory".into()
    }
}

#[cfg(test)]
mod tests {
    use super::Args;

    #[test]
    fn valid_category_parses() {
        let args: Args =
            serde_json::from_str(r#"{"content": "likes rust", "category": "preference"}"#).unwrap();
        assert_eq!(args.category, "preference");
        assert_eq!(args.content, "likes rust");
    }

    #[test]
    fn invalid_category_detected() {
        // The category validation happens in execute(), not in deserialization.
        // But we can verify that the string parses and then check manually.
        let args: Args =
            serde_json::from_str(r#"{"content": "test", "category": "bogus"}"#).unwrap();
        let valid = matches!(
            args.category.as_str(),
            "preference" | "fact" | "instruction"
        );
        assert!(!valid, "bogus should not be a valid category");
    }

    #[test]
    fn missing_required_field() {
        let result = serde_json::from_str::<Args>(r#"{"content": "test"}"#);
        assert!(result.is_err(), "missing 'category' should fail parsing");
    }
}
