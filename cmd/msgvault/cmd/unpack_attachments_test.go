package cmd

import (
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

// TestRefuseUnpackWithLiveDaemon pins the unpack preflight: unlike the SQLite
// write lock (which acquireDirectSQLiteWriteLock skips for PostgreSQL DSNs),
// this guard rejects unpack on every backend while any responding daemon owns
// the archive — including one whose API version is incompatible with this
// client, since it holds pack files open all the same.
func TestRefuseUnpackWithLiveDaemon(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()

	require.NoError(refuseUnpackWithLiveDaemon(dataDir),
		"no daemon: unpack preflight passes")

	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "v-test",
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: "v-test",
		Metadata: map[string]string{
			runtimeHost: host,
			runtimePort: portText,
			// An API version this client considers incompatible: the daemon
			// still holds pack files open, so it must be refused all the same.
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion + 1),
		},
	})
	require.NoError(err, "write runtime record")
	require.Nil(findDaemonRuntime(dataDir),
		"precondition: the daemon reads as incompatible to this client")

	err = refuseUnpackWithLiveDaemon(dataDir)
	require.ErrorContains(err, "msgvault serve stop",
		"live daemon (even incompatible) must be refused with actionable guidance")
}
