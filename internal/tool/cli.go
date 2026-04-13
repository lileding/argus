package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"argus/internal/config"
)

// CLITool executes commands in a Docker container for isolation.
type CLITool struct {
	cfg          config.DockerConfig
	workspaceDir string
}

func NewCLITool(cfg config.DockerConfig, workspaceDir string) *CLITool {
	return &CLITool{cfg: cfg, workspaceDir: workspaceDir}
}

func (t *CLITool) Name() string { return "cli" }

func (t *CLITool) Description() string {
	return "Execute a shell command inside a sandboxed Docker container. The workspace directory is mounted at /workspace. Use this for running scripts, data processing, or any computation."
}

func (t *CLITool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Shell command to execute"},
			"working_dir": {"type": "string", "description": "Working directory inside container (default: /workspace)"}
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

	if args.WorkingDir == "" {
		args.WorkingDir = "/workspace"
	}

	timeout := t.cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dockerArgs := []string{
		"run", "--rm",
		"--network", t.cfg.Network,
		"--memory", t.cfg.MemoryLimit,
		"--cpus", "1",
		"-v", t.workspaceDir + ":/workspace",
		"-w", args.WorkingDir,
		t.cfg.Image,
		"sh", "-c", args.Command,
	}

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	output, err := cmd.CombinedOutput()

	result := strings.TrimSpace(string(output))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("command timed out after %v", timeout)
		}
		return fmt.Sprintf("exit code: %v\noutput:\n%s", err, result), nil
	}

	return result, nil
}
