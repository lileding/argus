use std::path::Path;
use std::time::Duration;

use serde::Deserialize;
use serde_json::Value;

use super::Tool;

const TIMEOUT: Duration = Duration::from_secs(30);

pub(super) struct Cli<'a> {
    workspace_dir: &'a Path,
}

#[derive(Deserialize)]
struct Args {
    command: String,
    working_dir: Option<String>,
}

impl<'a> Cli<'a> {
    pub(super) fn new(workspace_dir: &'a Path) -> Self {
        Self { workspace_dir }
    }

    fn resolve_workdir(&self, raw: Option<&str>) -> Result<std::path::PathBuf, String> {
        let dir = match raw {
            Some(d) if !d.is_empty() => {
                let candidate = if Path::new(d).is_absolute() {
                    std::path::PathBuf::from(d)
                } else {
                    self.workspace_dir.join(d)
                };
                let ws = self
                    .workspace_dir
                    .canonicalize()
                    .map_err(|e| format!("cannot resolve workspace: {e}"))?;
                let resolved = candidate
                    .canonicalize()
                    .map_err(|e| format!("cannot resolve working_dir \"{d}\": {e}"))?;
                if !resolved.starts_with(&ws) {
                    return Err("working_dir is outside workspace".into());
                }
                resolved
            }
            _ => self.workspace_dir.to_path_buf(),
        };
        Ok(dir)
    }
}

#[async_trait::async_trait]
impl<'a> Tool for Cli<'a> {
    fn name(&self) -> &str {
        "cli"
    }

    fn description(&self) -> &str {
        "Execute a shell command on the host. Use this for running scripts, checking system state, \
         or performing operations that other tools cannot handle."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "command": {
                    "type": "string",
                    "description": "Shell command to execute"
                },
                "working_dir": {
                    "type": "string",
                    "description": "Working directory (default: workspace root)"
                }
            },
            "required": ["command"]
        })
    }

    async fn execute(&self, _ctx: &super::ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let workdir = match self.resolve_workdir(parsed.working_dir.as_deref()) {
            Ok(d) => d,
            Err(e) => return format!("error: {e}"),
        };

        let result = tokio::time::timeout(TIMEOUT, async {
            tokio::process::Command::new("bash")
                .arg("-c")
                .arg(&parsed.command)
                .current_dir(&workdir)
                .output()
                .await
        })
        .await;

        match result {
            Ok(Ok(output)) => {
                let stdout = String::from_utf8_lossy(&output.stdout);
                let stderr = String::from_utf8_lossy(&output.stderr);
                let combined = if stderr.is_empty() {
                    stdout.trim().to_string()
                } else if stdout.is_empty() {
                    stderr.trim().to_string()
                } else {
                    format!("{}\n{}", stdout.trim(), stderr.trim())
                };

                if output.status.success() {
                    combined
                } else {
                    let code = output.status.code().unwrap_or(-1);
                    format!("exit code: {code}\n{combined}")
                }
            }
            Ok(Err(e)) => format!("error: command failed: {e}"),
            Err(_) => "error: command timed out (30s)".into(),
        }
    }

    fn status_label(&self, args: &str) -> String {
        let cmd = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("command")?.as_str().map(String::from))
            .unwrap_or_default();
        if cmd.is_empty() {
            return "⚡ Running command".into();
        }
        format!("⚡ Running: {}", super::truncate_display(&cmd, 40))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn echo_command() {
        let dir = tempfile::tempdir().unwrap();
        let tool = Cli::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext { channel: "test" },
                r#"{"command": "echo hello"}"#,
            )
            .await;
        assert_eq!(result, "hello");
    }

    #[tokio::test]
    async fn nonzero_exit() {
        let dir = tempfile::tempdir().unwrap();
        let tool = Cli::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext { channel: "test" },
                r#"{"command": "exit 42"}"#,
            )
            .await;
        assert!(result.starts_with("exit code: 42"));
    }

    #[tokio::test]
    async fn working_dir_inside_workspace() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir(dir.path().join("sub")).unwrap();
        let tool = Cli::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext { channel: "test" },
                r#"{"command": "pwd", "working_dir": "sub"}"#,
            )
            .await;
        assert!(result.contains("sub"));
    }

    #[tokio::test]
    async fn working_dir_escape_rejected() {
        let dir = tempfile::tempdir().unwrap();
        let tool = Cli::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext { channel: "test" },
                r#"{"command": "pwd", "working_dir": "/tmp"}"#,
            )
            .await;
        assert!(
            result.starts_with("error:"),
            "expected error, got: {result}"
        );
    }
}
