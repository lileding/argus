package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"argus/internal/sandbox"
)

// CLITool executes shell commands via a sandbox.
// The tool defines WHAT to run; the sandbox decides WHERE to run it.
type CLITool struct {
	sandbox sandbox.Sandbox
}

func NewCLITool(sb sandbox.Sandbox) *CLITool {
	return &CLITool{sandbox: sb}
}

func (t *CLITool) Name() string { return "cli" }

func (t *CLITool) Description() string {
	return "Execute a shell command in the sandbox environment. The workspace directory is available for file operations. Use this for running scripts, data processing, or any computation."
}

func (t *CLITool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Shell command to execute"},
			"working_dir": {"type": "string", "description": "Working directory (default: workspace root)"}
		},
		"required": ["command"]
	}`)
}

type cliArgs struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir"`
}

func (t *CLITool) Execute(ctx context.Context, arguments string) (string, error) {
	var args cliArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	return t.sandbox.Exec(ctx, args.Command, args.WorkingDir)
}
