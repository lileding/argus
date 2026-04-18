package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspacePath resolves a path within the workspace, preventing directory traversal.
// Accepts both relative paths ("foo/bar.txt") and absolute paths that land within
// the workspace ("/Users/x/workspace/foo/bar.txt"). Rejects everything else.
func workspacePath(workspaceDir, path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("path %q uses ~, which is not supported. Use paths relative to the workspace (%s), or use the cli tool for files outside the workspace", path, workspaceDir)
	}

	cleaned := filepath.Clean(path)

	if filepath.IsAbs(cleaned) {
		// Allow absolute paths that resolve within the workspace.
		if strings.HasPrefix(cleaned, workspaceDir+string(filepath.Separator)) || cleaned == workspaceDir {
			return cleaned, nil
		}
		return "", fmt.Errorf("absolute path %q is outside the workspace (%s). Use paths relative to the workspace, or use the cli tool", path, workspaceDir)
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

// binaryDescs maps file extensions to human-readable descriptions.
// Files with these extensions are returned as a summary instead of raw bytes,
// since dumping binary content into a text-only LLM context is useless.
var binaryDescs = map[string]string{
	".png": "PNG image", ".jpg": "JPEG image", ".jpeg": "JPEG image",
	".gif": "GIF image", ".webp": "WebP image", ".bmp": "BMP image",
	".opus": "Opus audio", ".mp3": "MP3 audio", ".wav": "WAV audio",
	".ogg": "Ogg audio", ".m4a": "AAC audio",
	".mp4": "MP4 video", ".mov": "MOV video", ".avi": "AVI video",
	".zip": "ZIP archive", ".tar": "tar archive", ".gz": "gzip archive",
	".bin": "binary file",
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

	// Detect binary files by extension and return a description instead
	// of raw bytes — a text-only model cannot interpret binary content.
	ext := strings.ToLower(filepath.Ext(fullPath))
	if desc, ok := binaryDescs[ext]; ok {
		info, statErr := os.Stat(fullPath)
		if statErr != nil {
			return "", fmt.Errorf("read file: %w", statErr)
		}
		return fmt.Sprintf("Binary file: %s (%.1f KB). Cannot be read as text. "+
			"If this is an image the user sent, its content is described in the conversation history — "+
			"check previous user messages for a text description of what was in the image.",
			desc, float64(info.Size())/1024), nil
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
	return "Write content to a file. Files are saved under the .users/ directory in the workspace. Use this to save results, create scripts, or write reports."
}

func (t *WriteFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path (will be placed under .users/ directory)"},
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

	// Force all writes into .users/ directory — model cannot write to
	// .skills/, .files/, or other workspace directories.
	writePath := args.Path
	if !strings.HasPrefix(writePath, ".users/") && !strings.HasPrefix(writePath, ".users\\") {
		writePath = filepath.Join(".users", writePath)
	}

	fullPath, err := workspacePath(t.workspaceDir, writePath)
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
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), writePath), nil
}
