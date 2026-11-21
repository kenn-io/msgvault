//go:build !linux && !darwin

package peoplesweep

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

type unavailableCodexStarter struct{}

// No credential-bearing Codex process can launch on this platform.
func codexAuthOwnedByDaemon(os.FileInfo) bool { return true }

func NewCodexCommandStarter() CommandStarter { return unavailableCodexStarter{} }

func (unavailableCodexStarter) Start(context.Context, CodexExecutable, []string, []string, string) (RPCProcess, error) {
	return nil, errors.New("codex containment launcher is unavailable on this platform")
}

// codexVersionCommand runs the snapshot with a scrubbed environment. No
// platform here has a containment launcher, so no release can pass the gate.
func codexVersionCommand(ctx context.Context, verifiedPath, _ string) (*exec.Cmd, error) {
	command := exec.CommandContext(ctx, verifiedPath, "--version") //nolint:gosec // The gate owns this absolute private snapshot.
	command.Env = scrubCodexEnvironment(os.Environ())
	return command, nil
}
