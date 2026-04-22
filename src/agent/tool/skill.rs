use serde::Deserialize;
use serde_json::Value;

use super::Tool;
use crate::agent::skill::SkillIndex;

pub(super) struct ActivateSkill<'a> {
    index: &'a SkillIndex,
}

#[derive(Deserialize)]
struct Args {
    name: String,
}

impl<'a> ActivateSkill<'a> {
    pub(super) fn new(index: &'a SkillIndex) -> Self {
        Self { index }
    }
}

#[async_trait::async_trait]
impl<'a> Tool for ActivateSkill<'a> {
    fn name(&self) -> &str {
        "activate_skill"
    }

    fn description(&self) -> &str {
        "Load the full instructions for a skill by name. Use this when the user's request \
         matches one of the available skills listed in the system prompt."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "name": {
                    "type": "string",
                    "description": "Skill name from the catalog"
                }
            },
            "required": ["name"]
        })
    }

    async fn execute(&self, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        match self.index.get(&parsed.name) {
            Some(entry) => format!("## Skill: {}\n\n{}", entry.name, entry.prompt),
            None => format!("error: skill \"{}\" not found", parsed.name),
        }
    }

    fn status_label(&self, args: &str) -> String {
        let name = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("name")?.as_str().map(String::from))
            .unwrap_or_default();
        if name.is_empty() {
            return "📖 Loading skill".into();
        }
        format!("📖 Loading skill: {name}")
    }
}
