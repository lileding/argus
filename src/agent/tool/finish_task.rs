use serde_json::Value;

use super::Tool;

/// Signals that the orchestrator has finished its task. The orchestrator loop
/// intercepts this tool call before execution, so `execute` is unreachable.
pub(super) struct FinishTask;

#[async_trait::async_trait]
impl Tool for FinishTask {
    fn name(&self) -> &str {
        "finish_task"
    }

    fn description(&self) -> &str {
        "Call this tool when you have gathered all the information needed to answer the user's \
         question. Provide a summary of your findings."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "summary": {
                    "type": "string",
                    "description": "Summary of findings and materials gathered for the synthesizer."
                }
            },
            "required": ["summary"]
        })
    }

    async fn execute(&self, _args: &str) -> String {
        "error: finish_task should not be executed directly".into()
    }

    fn status_label(&self, _args: &str) -> String {
        "✅ Finishing".into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn name_is_finish_task() {
        let tool = FinishTask;
        assert_eq!(tool.name(), "finish_task");
    }

    #[test]
    fn parameters_require_summary() {
        let tool = FinishTask;
        let params = tool.parameters();
        let required = params["required"].as_array().unwrap();
        assert!(
            required.iter().any(|v| v.as_str() == Some("summary")),
            "expected 'summary' in required, got: {required:?}"
        );
        assert!(
            params["properties"]["summary"].is_object(),
            "expected 'summary' property to be defined"
        );
    }
}
