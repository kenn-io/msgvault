//go:build linux

package peoplesweep

import (
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dialCodexProxySession(t *testing.T, session CodexProxySession) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", session.Endpoint())
	require.NoError(t, err)
	return conn
}

// The owner-only socket admits only the daemon, so a CONNECT carries no credential.
func codexProxySessionCredential(*testing.T, CodexProxySession) string { return "" }

func assertCodexProxySessionClosed(t *testing.T, session CodexProxySession) {
	t.Helper()
	assert.NoFileExists(t, session.Endpoint())
}

func assertCodexProxySessionHeld(t *testing.T, session CodexProxySession) {
	t.Helper()
	info, err := os.Lstat(session.Endpoint())
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSocket)
}
