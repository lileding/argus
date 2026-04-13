package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"argus/internal/config"
)

// CLITool executes shell commands. In Docker mode (default), commands run inside
// a sandboxed container. In local mode (Image == "local"), commands run directly
// on the host, restricted to the workspace directory.
type CLITool struct {
	cfg          config.DockerConfig
	workspaceDir string
}

func NewCLITool(cfg config.DockerConfig, workspaceDir string) *CLITool {
	return &CLITool{cfg: cfg, workspaceDir: workspaceDir}
}

func (t *CLITool) Name() string { return "cli" }

func (t *CLITool) Description() string {
	return "Execute a shell command. The workspace directory is available at /workspace (Docker) or directly (local mode). Use this for running scripts, data processing, or any computation."
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

	timeout := t.cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Local mode: run directly on host.
	if t.cfg.Image == "local" {
		return t.executeLocal(ctx, args)
	}

	// Docker mode: run inside container.
	return t.executeDocker(ctx, args)
}

func (t *CLITool) executeLocal(ctx context.Context, args cliArgs) (string, error) {
	workDir := t.workspaceDir
	if args.WorkingDir != "" {
		workDir = filepath.Join(t.workspaceDir, filepath.Clean(args.WorkingDir))
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()

	result := strings.TrimSpace(string(output))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("command timed out")
		}
		return fmt.Sprintf("exit code: %v\noutput:\n%s", err, result), nil
	}
	return result, nil
}

func (t *CLITool) executeDocker(ctx context.Context, args cliArgs) (string, error) {
	if args.WorkingDir == "" {
		args.WorkingDir = "/workspace"
	}

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
			return "", fmt.Errorf("command timed out")
		}
		return fmt.Sprintf("exit code: %v\noutput:\n%s", err, result), nil
	}
	return result, nil
}
