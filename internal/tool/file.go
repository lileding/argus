package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileTool provides file read/write operations restricted to a workspace directory.
type FileTool struct {
	workspaceDir string
}

func NewFileTool(workspaceDir string) *FileTool {
	abs, err := filepath.Abs(workspaceDir)
	if err != nil {
		abs = workspaceDir
	}
	return &FileTool{workspaceDir: abs}
}

func (t *FileTool) Name() string { return "file" }

func (t *FileTool) Description() string {
	return "Read or write files within the workspace directory. Use action 'read' to read a file, 'write' to write content to a file."
}

func (t *FileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {"type": "string", "enum": ["read", "write"], "description": "read or write"},
			"path": {"type": "string", "description": "File path relative to workspace"},
			"content": {"type": "string", "description": "Content to write (only for write action)"}
		},
		"required": ["action", "path"]
	}`)
}

type fileArgs struct {
	Action  string `json:"action"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *FileTool) Execute(_ context.Context, arguments string) (string, error) {
	var args fileArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	fullPath, err := t.resolvePath(args.Path)
	if err != nil {
		return "", err
	}

	switch args.Action {
	case "read":
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
		return string(data), nil

	case "write":
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("create directory: %w", err)
		}
		if err := os.WriteFile(fullPath, []byte(args.Content), 0644); err != nil {
			return "", fmt.Errorf("write file: %w", err)
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil

	default:
		return "", fmt.Errorf("unknown action: %s", args.Action)
	}
}

// resolvePath resolves a relative path within the workspace, preventing directory traversal.
func (t *FileTool) resolvePath(path string) (string, error) {
	// Clean the path to resolve ".." etc.
	cleaned := filepath.Clean(path)

	// Reject absolute paths.
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths not allowed: %s", path)
	}

	// Reject paths that escape the workspace.
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path escapes workspace: %s", path)
	}

	full := filepath.Join(t.workspaceDir, cleaned)

	// Double-check the resolved path is within workspace.
	if !strings.HasPrefix(full, t.workspaceDir) {
		return "", fmt.Errorf("path escapes workspace: %s", path)
	}

	return full, nil
}
