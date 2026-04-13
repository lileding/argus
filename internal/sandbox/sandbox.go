package sandbox

import "context"

// Sandbox is the execution environment for shell commands.
// Tool layer decides WHAT to run; sandbox decides WHERE to run it.
type Sandbox interface {
	Exec(ctx context.Context, command string, workDir string) (string, error)
}
