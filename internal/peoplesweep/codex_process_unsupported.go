//go:build !linux

package peoplesweep

import (
	"context"
	"errors"
	"os"
)

type unavailableCodexStarter struct{}

// No credential-bearing Codex process can launch on this platform.
func codexAuthOwnedByDaemon(os.FileInfo) bool { return true }

func NewCodexCommandStarter() CommandStarter { return unavailableCodexStarter{} }

func (unavailableCodexStarter) Start(context.Context, CodexExecutable, []string, []string, string) (RPCProcess, error) {
	return nil, errors.New("codex containment launcher is unavailable on this platform")
}
