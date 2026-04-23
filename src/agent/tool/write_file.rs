use std::path::Path;

use serde::Deserialize;
use serde_json::Value;

use crate::config::USER_DIR;

use super::Tool;

pub(super) struct WriteFile<'a> {
    workspace_dir: &'a Path,
}

#[derive(Deserialize)]
struct Args {
    path: String,
    content: String,
}

impl<'a> WriteFile<'a> {
    pub(super) fn new(workspace_dir: &'a Path) -> Self {
        Self { workspace_dir }
    }
}

#[async_trait::async_trait]
impl<'a> Tool for WriteFile<'a> {
    fn name(&self) -> &str {
        "write_file"
    }

    fn description(&self) -> &str {
        "Write content to a file. The file is always created under the .users/ directory in \
         the workspace. Provide a relative path (e.g. \"notes/todo.txt\")."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "File path relative to the .users/ directory."
                },
                "content": {
                    "type": "string",
                    "description": "Content to write to the file."
                }
            },
            "required": ["path", "content"]
        })
    }

    async fn execute(&self, _ctx: &super::ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        // Force all writes under USER_DIR.
        let user_dir = self.workspace_dir.join(USER_DIR);

        // Reject obvious traversal before any filesystem side-effects.
        let clean = std::path::Path::new(&parsed.path);
        for comp in clean.components() {
            if matches!(comp, std::path::Component::ParentDir) {
                return "error: path escapes the user directory".into();
            }
        }

        let target = user_dir.join(clean);

        // Create parent directories.
        if let Some(parent) = target.parent()
            && let Err(e) = tokio::fs::create_dir_all(parent).await
        {
            return format!("error: failed to create directories: {e}");
        }

        // Verify the resolved path stays under user_dir after creation.
        let user_dir_canonical = match user_dir.canonicalize() {
            Ok(p) => p,
            Err(e) => return format!("error: cannot resolve user directory: {e}"),
        };
        let parent_canonical = match target.parent().unwrap_or(&user_dir).canonicalize() {
            Ok(p) => p,
            Err(e) => return format!("error: cannot resolve target directory: {e}"),
        };
        if !parent_canonical.starts_with(&user_dir_canonical) {
            return "error: path escapes the user directory".into();
        }

        let bytes = parsed.content.as_bytes();
        match tokio::fs::write(&target, bytes).await {
            Ok(()) => format!("wrote {} bytes to {}", bytes.len(), parsed.path),
            Err(e) => format!("error: failed to write file: {e}"),
        }
    }

    fn status_label(&self, args: &str) -> String {
        let path = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("path")?.as_str().map(String::from))
            .unwrap_or_default();
        if path.is_empty() {
            return "📝 Writing file".into();
        }
        format!("📝 Writing: {}", super::truncate_display(&path, 40))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn writes_to_users_subdirectory() {
        let dir = tempfile::tempdir().unwrap();
        let tool = WriteFile::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext {
                    channel: "test",
                    msg_id: "test",
                    port: &tokio::sync::mpsc::channel(1).0,
                },
                r#"{"path": "notes.txt", "content": "hello"}"#,
            )
            .await;
        assert!(
            result.starts_with("wrote"),
            "expected success, got: {result}"
        );
        let written = std::fs::read_to_string(dir.path().join(".users/notes.txt")).unwrap();
        assert_eq!(written, "hello");
    }

    #[tokio::test]
    async fn creates_parent_directories() {
        let dir = tempfile::tempdir().unwrap();
        let tool = WriteFile::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext {
                    channel: "test",
                    msg_id: "test",
                    port: &tokio::sync::mpsc::channel(1).0,
                },
                r#"{"path": "sub/deep/file.txt", "content": "nested"}"#,
            )
            .await;
        assert!(
            result.starts_with("wrote"),
            "expected success, got: {result}"
        );
        let written = std::fs::read_to_string(dir.path().join(".users/sub/deep/file.txt")).unwrap();
        assert_eq!(written, "nested");
    }

    #[tokio::test]
    async fn content_written_correctly() {
        let dir = tempfile::tempdir().unwrap();
        let tool = WriteFile::new(dir.path());
        let content = "line1\nline2\n中文内容";
        let args = serde_json::json!({"path": "test.txt", "content": content}).to_string();
        let result = tool
            .execute(
                &super::super::ToolContext {
                    channel: "test",
                    msg_id: "test",
                    port: &tokio::sync::mpsc::channel(1).0,
                },
                &args,
            )
            .await;
        assert!(result.starts_with("wrote"));
        let written = std::fs::read_to_string(dir.path().join(".users/test.txt")).unwrap();
        assert_eq!(written, content);
    }

    #[tokio::test]
    async fn path_escape_attempt_fails() {
        let dir = tempfile::tempdir().unwrap();
        let tool = WriteFile::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext {
                    channel: "test",
                    msg_id: "test",
                    port: &tokio::sync::mpsc::channel(1).0,
                },
                r#"{"path": "../../etc/evil.txt", "content": "pwned"}"#,
            )
            .await;
        assert!(
            result.starts_with("error:"),
            "expected error for path escape, got: {result}"
        );
    }
}
