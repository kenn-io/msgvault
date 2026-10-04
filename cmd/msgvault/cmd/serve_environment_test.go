package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func TestRunServeRejectedContenderKeepsRunningDaemonKey(t *testing.T) { //nolint:paralleltest // process environment and daemon lifecycle
	require := require.New(t)
	clearServerKeyEnvironment(t)
	t.Setenv("MSGVAULT_REMOTE_URL", "")
	t.Setenv("MSGVAULT_ALLOW_INSECURE", "false")
	home := t.TempDir()
	port := freeTCPPort(t)
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte(fmt.Sprintf("[server]\nbind_addr = '127.0.0.1'\napi_port = %d\ndaemon_auto_start = false\ndaemon_auto_restart = 'never'\n[analytics]\nengine = 'sql'\n", port)), 0o600))
	running, err := config.Load(path, home)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	command := &cobra.Command{Use: "serve"}
	command.SetContext(testInvocationContext(ctx, running, invocationOptions{}))
	done := make(chan error, 1)
	go func() { done <- runServe(command, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(err)
		case <-time.After(serveLifecycleTestTimeout):
			require.Fail("daemon did not stop")
		}
	})
	waitForServeHealthBounded(t, port, done)
	initial, _, err := OpenHTTPStore(withStoreResolverConfig(t, running))
	require.NoError(err)
	require.NoError(initial.Close())

	contender, err := config.LoadWithOverrides(path, home, config.RuntimeOverrides{BindAddr: new("0.0.0.0"), APIPort: new(0)})
	require.NoError(err)
	contenderCommand := &cobra.Command{Use: "serve"}
	contenderCommand.SetContext(testInvocationContext(t.Context(), contender, invocationOptions{}))
	err = runServe(contenderCommand, nil)
	require.ErrorAs(err, &daemonOwnerLockHeldError{})
	_, err = os.Stat(contender.ServerKeyFilePath())
	require.ErrorIs(err, os.ErrNotExist, "a rejected contender must not create a new daemon credential")

	fresh, err := config.Load(path, home)
	require.NoError(err)
	client, _, err := OpenHTTPStore(withStoreResolverConfig(t, fresh))
	require.NoError(err, "the running daemon must remain usable by fresh local clients")
	require.NoError(client.Close())
}

func TestRunServeCredentialFailureReleasesStartupResources(t *testing.T) {
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Server.APIPort = freeTCPPort(t)
	cfg.Server.APIKeyFile = filepath.Join(cfg.HomeDir, "missing-key")
	command := &cobra.Command{Use: "serve"}
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.ErrorIs(runServe(command, nil), os.ErrNotExist)

	daemonLock, err := tryAcquireDaemonOwnerLock(cfg.Data.DataDir)
	require.NoError(err, "credential failure must release daemon ownership")
	t.Cleanup(func() { require.NoError(daemonLock.Close()) })
	writeLock, err := tryAcquireWriteOwnerLock(cfg.Data.DataDir)
	require.NoError(err, "credential failure must release archive ownership")
	t.Cleanup(func() { require.NoError(writeLock.Close()) })
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(cfg.Server.APIPort)))
	require.NoError(err, "credential failure must release the reserved listener")
	require.NoError(listener.Close())
	records, err := daemonRuntimeStore(cfg.Data.DataDir).List()
	require.NoError(err)
	require.Empty(records, "failed credential preparation must not publish a runtime record")
}

func TestRunServeEnvironmentOnlyAuthenticatedRestart(t *testing.T) { //nolint:paralleltest // process environment and daemon lifecycle
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	port := freeTCPPort(t)
	t.Setenv("MSGVAULT_HOME", home)
	t.Setenv("MSGVAULT_BIND_ADDR", "0.0.0.0")
	t.Setenv("MSGVAULT_API_PORT", strconv.Itoa(port))
	var firstKey string
	for start := range 2 {
		cfg, err := config.Load("", home)
		require.NoError(err)
		ctx, cancel := context.WithCancel(t.Context())
		command := &cobra.Command{Use: "serve"}
		command.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
		done := make(chan error, 1)
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			cancel()
			select {
			case err := <-done:
				require.NoError(err)
				stopped = true
			case <-time.After(serveLifecycleTestTimeout):
				assert.Fail("daemon did not stop")
			}
		}
		t.Cleanup(stop)
		go func() { done <- runServe(command, nil) }()
		waitForServeHealthBounded(t, port, done)
		key, err := providercredentials.ReadSecretFile(cfg.ServerKeyFilePath())
		require.NoError(err)
		if start == 0 {
			firstKey = key
		} else {
			assert.Equal(firstKey, key)
		}
		_, err = os.Stat(filepath.Join(home, "config.toml"))
		require.ErrorIs(err, os.ErrNotExist)
		client := &http.Client{Timeout: serveLifecycleTestTimeout}
		var status int
		require.Eventually(func() bool {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port), nil)
			if err != nil {
				return false
			}
			request.Header.Set("X-Api-Key", key)
			response, err := client.Do(request)
			if err != nil {
				return false
			}
			status = response.StatusCode
			_ = response.Body.Close()
			return status == http.StatusOK
		}, serveLifecycleTestTimeout, 20*time.Millisecond)
		// Startup polling can consume the public request burst. Wait for the
		// unauthenticated request to reach auth after the limiter replenishes.
		require.Eventually(func() bool {
			response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port))
			if err != nil {
				return false
			}
			status = response.StatusCode
			_ = response.Body.Close()
			return status == http.StatusUnauthorized
		}, serveLifecycleTestTimeout, 200*time.Millisecond)
		var output bytes.Buffer
		statusConfig, err := config.Load("", home)
		require.NoError(err)
		statusCommand := newLifecycleCommand("status", false)
		statusCommand.SetContext(testInvocationContext(ctx, statusConfig, invocationOptions{}))
		statusCommand.SetOut(&output)
		statusCommand.SetErr(io.Discard)
		require.Eventually(func() bool {
			output.Reset()
			return statusCommand.RunE(statusCommand, nil) == nil && strings.Contains(output.String(), strconv.Itoa(port))
		}, serveLifecycleTestTimeout, 200*time.Millisecond)
		assert.NotContains(output.String(), key)
		stop()
	}
}
