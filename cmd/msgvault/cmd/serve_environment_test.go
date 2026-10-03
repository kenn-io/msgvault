package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
