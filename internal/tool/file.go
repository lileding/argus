package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspacePath resolves a relative path within the workspace, preventing directory traversal.
func workspacePath(workspaceDir, path string) (string, error) {
	// Expand ~ to indicate it's not supported here.
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("path %q uses ~, which is not supported. Use paths relative to the workspace (%s), or use the cli tool for files outside the workspace", path, workspaceDir)
	}

	cleaned := filepath.Clean(path)

	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute path %q not allowed. Use paths relative to the workspace (%s), or use the cli tool (e.g. cat %s) for files outside the workspace", path, workspaceDir, path)
	}
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path %q escapes workspace (%s). Use the cli tool for files outside the workspace", path, workspaceDir)
	}

	full := filepath.Join(workspaceDir, cleaned)
	if !strings.HasPrefix(full, workspaceDir) {
		return "", fmt.Errorf("path %q escapes workspace (%s)", path, workspaceDir)
	}

	return full, nil
}

// --- ReadFileTool ---

type ReadFileTool struct {
	workspaceDir string
}

func NewReadFileTool(workspaceDir string) *ReadFileTool {
	abs, _ := filepath.Abs(workspaceDir)
	return &ReadFileTool{workspaceDir: abs}
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "Read the contents of a file in the workspace. Use this to examine files, summarize content, or check data."
}

func (t *ReadFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to workspace"}
		},
		"required": ["path"]
	}`)
}

type readFileArgs struct {
	Path string `json:"path"`
}

func (t *ReadFileTool) Execute(_ context.Context, arguments string) (string, error) {
	var args readFileArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	fullPath, err := workspacePath(t.workspaceDir, args.Path)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	return string(data), nil
}

// --- WriteFileTool ---

type WriteFileTool struct {
	workspaceDir string
}

func NewWriteFileTool(workspaceDir string) *WriteFileTool {
	abs, _ := filepath.Abs(workspaceDir)
	return &WriteFileTool{workspaceDir: abs}
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Description() string {
	return "Write content to a file in the workspace. Creates the file and any parent directories if they don't exist. Use this to save results, create scripts, or write reports."
}

func (t *WriteFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to workspace"},
			"content": {"type": "string", "description": "Content to write"}
		},
		"required": ["path", "content"]
	}`)
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFileTool) Execute(_ context.Context, arguments string) (string, error) {
	var args writeFileArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	fullPath, err := workspacePath(t.workspaceDir, args.Path)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create directory: %w", err)
	}
	if err := os.WriteFile(fullPath, []byte(args.Content), 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil
}
