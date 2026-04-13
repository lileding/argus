package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Local executes commands directly on the host, restricted to the workspace directory.
type Local struct {
	WorkspaceDir string
	Timeout      time.Duration
}

func (s *Local) Exec(ctx context.Context, command string, workDir string) (string, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := s.WorkspaceDir
	if workDir != "" {
		dir = filepath.Join(s.WorkspaceDir, filepath.Clean(workDir))
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
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
