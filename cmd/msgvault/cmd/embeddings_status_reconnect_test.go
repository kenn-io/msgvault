package cmd

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
)

func TestReconnectLocalDaemonFollowsKeyRotationAndPortChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: "v-test"}))
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err)
	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID: os.Getpid(), Network: daemon.NetworkTCP, Address: net.JoinHostPort(host, port),
		Service: daemonService, Version: "v-test",
		Metadata: map[string]string{
			runtimeHost: host, runtimePort: port,
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
			runtimeCreateTime:       matchingProcessCreateTime(t),
		},
	})
	require.NoError(err)
	keyFile := filepath.Join(t.TempDir(), "server.key")
	// Credential files must be private; SecureReplaceFile sets that on Windows too.
	require.NoError(fileutil.SecureReplaceFile(keyFile, []byte("key-one"), 0o600))
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dataDir
	cfg.Server.APIKeyFile = keyFile
	require.NoError(cfg.ResolveServerKey())
	ctx := withInvocation(t.Context(), &invocation{cfg: cfg})
	rt := findDaemonRuntime(dataDir)
	require.NotNil(rt)
	current := localDaemonEndpoint{url: urlFromDaemonRuntime(rt), key: "key-one"}

	client, endpoint, err := reconnectLocalDaemon(ctx, current)
	require.NoError(err)
	assert.Nil(client, "the same daemon and key keep the current client")
	assert.Equal(current, endpoint)

	require.NoError(fileutil.SecureReplaceFile(keyFile, []byte("key-two"), 0o600))
	client, endpoint, err = reconnectLocalDaemon(ctx, current)
	require.NoError(err)
	require.NotNil(client, "a rotated key rebuilds the client on the same port")
	t.Cleanup(func() { _ = client.Close() })
	assert.Equal(localDaemonEndpoint{url: current.url, key: "key-two"}, endpoint)

	moved, movedEndpoint, err := reconnectLocalDaemon(ctx, localDaemonEndpoint{url: "http://127.0.0.1:1", key: "key-two"})
	require.NoError(err)
	require.NotNil(moved, "a daemon on a new port gets a new client")
	t.Cleanup(func() { _ = moved.Close() })
	assert.Equal(current.url, movedEndpoint.url)
}
