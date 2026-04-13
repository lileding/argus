package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Docker executes commands inside a Docker container.
type Docker struct {
	Image        string
	WorkspaceDir string
	Network      string
	MemoryLimit  string
	Timeout      time.Duration
}

func (s *Docker) Exec(ctx context.Context, command string, workDir string) (string, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if workDir == "" {
		workDir = "/workspace"
	}

	network := s.Network
	if network == "" {
		network = "none"
	}
	memory := s.MemoryLimit
	if memory == "" {
		memory = "512m"
	}

	args := []string{
		"run", "--rm",
		"--network", network,
		"--memory", memory,
		"--cpus", "1",
		"-v", s.WorkspaceDir + ":/workspace",
		"-w", workDir,
		s.Image,
		"sh", "-c", command,
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
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
