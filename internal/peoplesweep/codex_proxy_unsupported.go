//go:build !linux

package peoplesweep

import (
	"context"
	"errors"
)

type unavailableCodexProxy struct{}

func defaultCodexServiceProxy() CodexServiceProxy { return unavailableCodexProxy{} }

func (unavailableCodexProxy) Attach(context.Context, string) (CodexProxySession, error) {
	return nil, errors.New("codex service proxy is unavailable on this platform")
}
