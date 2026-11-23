//go:build linux

package peoplesweep

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
)

const codexProxySocketName = ".proxy.sock"

// listenCodexProxy serves the policy proxy on an owner-only Unix socket in the
// work root. Only the daemon and the bridge inside the child's empty network
// namespace can reach it, so CONNECT requests carry no credential.
func listenCodexProxy(ctx context.Context, workRoot string) (net.Listener, string, string, error) {
	socketPath := filepath.Join(workRoot, codexProxySocketName)
	if _, err := os.Lstat(socketPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil, "", "", errors.New("codex proxy socket path is occupied")
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	if err != nil {
		return nil, "", "", errors.New("create codex service proxy socket")
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, "", "", errors.New("secure codex service proxy socket")
	}
	return listener, socketPath, "", nil
}
