mod cancel_cron;
mod cli;
mod create_cron;
mod create_task;
mod current_time;
mod db;
mod fetch;
mod finish_task;
mod forget;
mod list_crons;
mod list_docs;
mod read_file;
mod remember;
mod search;
mod search_docs;
mod search_history;
mod skill;
mod update_cron;
mod write_file;

use std::path::Path;

use serde_json::Value;

use crate::agent::skill::SkillIndex;
use crate::database::Database;

use super::EmbedService;

/// Execution context passed to every tool call.
#[derive(Clone)]
pub(super) struct ToolContext<'a> {
    pub(super) channel: &'a str,
    pub(super) msg_id: &'a str,
    pub(super) port: &'a tokio::sync::mpsc::Sender<super::Notification>,
}

/// A tool that the orchestrator can invoke.
#[async_trait::async_trait]
pub(super) trait Tool: Send + Sync {
    /// Tool name (matches the function name in the model schema).
    fn name(&self) -> &str;

    /// Human-readable description for the model.
    fn description(&self) -> &str;

    /// JSON Schema for the tool's parameters.
    fn parameters(&self) -> Value;

    /// Execute the tool with the given JSON arguments. Returns a human-readable
    /// string. Errors are returned as `"error: ..."` — never panics.
    async fn execute(&self, ctx: &ToolContext<'_>, args: &str) -> String;

    /// Status line shown on the card while this tool is running.
    fn status_label(&self, args: &str) -> String;

    /// Normalized form of arguments for trace storage.
    /// Default: returns args unchanged. Override for tools like `db`
    /// that have structured command syntax worth normalizing.
    fn normalize_args(&self, args: &str) -> String {
        args.to_string()
    }
}

/// Registry of available tools.
pub(super) struct ToolRegistry<'a> {
    tools: Vec<Box<dyn Tool + 'a>>,
}

impl<'a> ToolRegistry<'a> {
    /// Look up a tool by name.
    pub(super) fn get(&self, name: &str) -> Option<&dyn Tool> {
        self.tools
            .iter()
            .find(|t| t.name() == name)
            .map(|t| t.as_ref())
    }

    /// Iterate over all registered tools.
    pub(super) fn iter(&self) -> impl Iterator<Item = &dyn Tool> {
        self.tools.iter().map(|t| t.as_ref())
    }
}

/// Truncate a string to max chars, appending "..." if truncated.
fn truncate_display(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        s.to_string()
    } else {
        let t: String = s.chars().take(max).collect();
        format!("{t}...")
    }
}

/// Build the full tool registry with all available tools.
#[allow(clippy::too_many_arguments)]
pub(super) fn build_registry<'a, E: EmbedService>(
    db: &'a Database,
    embed_service: &'a E,
    workspace_dir: &'a Path,
    http: &'a reqwest::Client,
    tavily_api_key: &'a str,
    skill_index: &'a SkillIndex,
    task_tx: &'a tokio::sync::mpsc::Sender<super::TaskSpec>,
    next_task_id: &'a std::sync::atomic::AtomicU32,
    include_create_task: bool,
) -> ToolRegistry<'a> {
    let mut tools: Vec<Box<dyn Tool + 'a>> = vec![
        Box::new(finish_task::FinishTask),
        Box::new(current_time::CurrentTime),
        Box::new(search::Search::new(http, tavily_api_key)),
        Box::new(fetch::Fetch::new(http)),
        Box::new(read_file::ReadFile::new(workspace_dir)),
        Box::new(write_file::WriteFile::new(workspace_dir)),
        Box::new(cli::Cli::new(workspace_dir)),
        Box::new(remember::Remember::new(db)),
        Box::new(forget::Forget::new(db)),
        Box::new(search_docs::SearchDocs::new(db, embed_service)),
        Box::new(list_docs::ListDocs::new(db)),
        Box::new(search_history::SearchHistory::new(db, embed_service)),
        Box::new(db::Db::new(db)),
        Box::new(skill::ActivateSkill::new(skill_index)),
        // Cron management is always available so cron-triggered tasks can
        // self-cancel (one-shot reminder pattern) or modify themselves.
        Box::new(list_crons::ListCrons::new(db)),
        Box::new(cancel_cron::CancelCron::new(db)),
        Box::new(update_cron::UpdateCron::new(db)),
    ];
    if include_create_task {
        // Creation tools only in user-facing sync path — prevents recursive
        // task/cron creation from inside async tasks or cron firings.
        tools.push(Box::new(create_task::CreateTask::new(
            task_tx,
            next_task_id,
        )));
        tools.push(Box::new(create_cron::CreateCron::new(db)));
    }

    ToolRegistry { tools }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[allow(dead_code)]
    struct MockEmbedService;

    #[async_trait::async_trait]
    impl EmbedService for MockEmbedService {
        async fn embed_one(
            &self,
            _text: &str,
        ) -> Result<Vec<f32>, Box<dyn std::error::Error + Send + Sync>> {
            Ok(vec![0.0; 768])
        }

        fn model_name(&self) -> &str {
            "mock"
        }
    }

    // We cannot construct a real Database in unit tests (requires Postgres).
    // These tests are guarded behind a feature or skipped if DB is unavailable.
    // For now, test the parts that don't require Database by checking tool
    // trait implementations directly.

    #[test]
    fn finish_task_in_registry_name() {
        // Verify FinishTask tool works standalone.
        let tool = finish_task::FinishTask;
        assert_eq!(tool.name(), "finish_task");
    }

    #[test]
    fn current_time_in_registry_name() {
        let tool = current_time::CurrentTime;
        assert_eq!(tool.name(), "current_time");
    }

    #[test]
    fn fetch_tool_name() {
        let http = reqwest::Client::new();
        let tool = fetch::Fetch::new(&http);
        assert_eq!(tool.name(), "fetch");
    }

    #[test]
    fn search_tool_name() {
        let http = reqwest::Client::new();
        let tool = search::Search::new(&http, "fake-key");
        assert_eq!(tool.name(), "search");
    }

    #[test]
    fn read_file_tool_name() {
        let dir = tempfile::tempdir().unwrap();
        let tool = read_file::ReadFile::new(dir.path());
        assert_eq!(tool.name(), "read_file");
    }

    #[test]
    fn write_file_tool_name() {
        let dir = tempfile::tempdir().unwrap();
        let tool = write_file::WriteFile::new(dir.path());
        assert_eq!(tool.name(), "write_file");
    }
}
