use std::path::{Path, PathBuf};

use serde::Deserialize;
use serde_json::Value;

use super::Tool;

/// Binary file extensions that should not be read as text.
const BINARY_EXTENSIONS: &[&str] = &[
    ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".opus", ".mp3", ".wav", ".ogg", ".m4a",
    ".mp4", ".mov", ".avi", ".zip", ".tar", ".gz", ".bin",
];

pub(super) struct ReadFile<'a> {
    workspace_dir: &'a Path,
}

#[derive(Deserialize)]
struct Args {
    path: String,
}

impl<'a> ReadFile<'a> {
    pub(super) fn new(workspace_dir: &'a Path) -> Self {
        Self { workspace_dir }
    }

    /// Resolve and validate that the path stays within the workspace.
    fn resolve_path(&self, raw: &str) -> Result<PathBuf, String> {
        // Reject obvious escape attempts.
        if raw.starts_with('~') {
            return Err("path must not start with ~".into());
        }

        let candidate = if Path::new(raw).is_absolute() {
            PathBuf::from(raw)
        } else {
            self.workspace_dir.join(raw)
        };

        // Canonicalize workspace first.
        let ws_canonical = self
            .workspace_dir
            .canonicalize()
            .map_err(|e| format!("cannot resolve workspace: {e}"))?;

        // For the candidate, resolve parent first (file may not exist yet for
        // other tools, but for read it must exist).
        let resolved = candidate
            .canonicalize()
            .map_err(|e| format!("cannot resolve path \"{raw}\": {e}"))?;

        if !resolved.starts_with(&ws_canonical) {
            return Err(format!(
                "access denied: path resolves outside workspace ({})",
                ws_canonical.display()
            ));
        }

        Ok(resolved)
    }
}

#[async_trait::async_trait]
impl<'a> Tool for ReadFile<'a> {
    fn name(&self) -> &str {
        "read_file"
    }

    fn description(&self) -> &str {
        "Read the contents of a file in the workspace. Returns the file content as text. \
         Binary files (images, audio, video, archives) are rejected."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "File path relative to workspace, or absolute within workspace."
                }
            },
            "required": ["path"]
        })
    }

    async fn execute(&self, _ctx: &super::ToolContext<'_>, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let path = match self.resolve_path(&parsed.path) {
            Ok(p) => p,
            Err(e) => return format!("error: {e}"),
        };

        // Check binary extensions.
        let path_lower = path.to_string_lossy().to_lowercase();
        for ext in BINARY_EXTENSIONS {
            if path_lower.ends_with(ext) {
                return format!(
                    "error: binary file detected ({}). Cannot read as text.",
                    ext
                );
            }
        }

        // Read the file.
        let content = match tokio::fs::read(&path).await {
            Ok(c) => c,
            Err(e) => return format!("error: {e}"),
        };

        // Check for null bytes in the first 8KB (binary detection).
        let check_len = content.len().min(8192);
        if content[..check_len].contains(&0) {
            return "error: file appears to be binary (contains null bytes)".into();
        }

        match String::from_utf8(content) {
            Ok(text) => text,
            Err(_) => "error: file is not valid UTF-8".into(),
        }
    }

    fn status_label(&self, args: &str) -> String {
        let path = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("path")?.as_str().map(String::from))
            .unwrap_or_default();
        if path.is_empty() {
            return "📄 Reading file".into();
        }
        format!("📄 Reading: {}", super::truncate_display(&path, 40))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // --- resolve_path ---

    #[test]
    fn resolve_path_rejects_tilde() {
        let dir = tempfile::tempdir().unwrap();
        let tool = ReadFile::new(dir.path());
        let result = tool.resolve_path("~/secret");
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("must not start with ~"));
    }

    #[test]
    fn resolve_path_relative_within_workspace() {
        let dir = tempfile::tempdir().unwrap();
        let file_path = dir.path().join("test.txt");
        std::fs::write(&file_path, "hello").unwrap();

        let tool = ReadFile::new(dir.path());
        let resolved = tool.resolve_path("test.txt").unwrap();
        assert_eq!(resolved, file_path.canonicalize().unwrap());
    }

    #[test]
    fn resolve_path_escape_attempt() {
        let dir = tempfile::tempdir().unwrap();
        // Create a file outside workspace to ensure canonicalize can resolve it.
        let tool = ReadFile::new(dir.path());
        let result = tool.resolve_path("../../../etc/passwd");
        // Either "cannot resolve" (file doesn't exist) or "access denied" (outside workspace).
        assert!(result.is_err());
    }

    // --- execute ---

    #[tokio::test]
    async fn execute_read_text_file() {
        let dir = tempfile::tempdir().unwrap();
        let file_path = dir.path().join("hello.txt");
        std::fs::write(&file_path, "hello world").unwrap();

        let tool = ReadFile::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext { channel: "test" },
                &format!(r#"{{"path": "hello.txt"}}"#),
            )
            .await;
        assert_eq!(result, "hello world");
    }

    #[tokio::test]
    async fn execute_binary_extension_detection() {
        let dir = tempfile::tempdir().unwrap();
        let file_path = dir.path().join("image.png");
        std::fs::write(&file_path, "fake png data").unwrap();

        let tool = ReadFile::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext { channel: "test" },
                &format!(r#"{{"path": "image.png"}}"#),
            )
            .await;
        assert!(
            result.starts_with("error:"),
            "expected error for binary file, got: {result}"
        );
        assert!(result.contains("binary file detected"));
    }

    #[tokio::test]
    async fn execute_nonexistent_file() {
        let dir = tempfile::tempdir().unwrap();
        let tool = ReadFile::new(dir.path());
        let result = tool
            .execute(
                &super::super::ToolContext { channel: "test" },
                r#"{"path": "does_not_exist.txt"}"#,
            )
            .await;
        assert!(
            result.starts_with("error:"),
            "expected error for nonexistent file, got: {result}"
        );
    }
}
