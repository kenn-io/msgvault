package cmd

import (
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
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

func TestRunUnpackAttachmentsLocalRejectsConfiguredRemote(t *testing.T) {
	require := require.New(t)
	savedCfg := cfg
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})

	dataDir := t.TempDir()
	cfg = &config.Config{
		Data:   config.DataConfig{DataDir: dataDir},
		Remote: config.RemoteConfig{URL: "https://vault.example.com"},
	}
	useLocal = false
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)

	err := runUnpackAttachmentsLocal(cmd)
	require.ErrorContains(err, "local-only")
	require.ErrorContains(err, "archive host")
	_, statErr := os.Stat(cfg.DatabaseDSN())
	require.ErrorIs(statErr, os.ErrNotExist,
		"remote refusal must happen before initializing a local archive")
}
