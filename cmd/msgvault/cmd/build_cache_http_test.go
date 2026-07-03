package cmd

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestBuildCacheUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/build-cache", r.URL.Path, "path")
		assert.Equal("true", r.URL.Query().Get("full_rebuild"), "full_rebuild query")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Building cache...\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"Warning: using CSV fallback\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Exported 42 messages to /tmp/msgvault-analytics\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote: config.RemoteConfig{
			URL:           server.URL,
			AllowInsecure: true,
		},
	})
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	t.Cleanup(func() { logger = oldLogger })

	oldFullRebuild := fullRebuild
	fullRebuild = false
	t.Cleanup(func() { fullRebuild = oldFullRebuild })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: buildCacheCmd.Use, RunE: buildCacheCmd.RunE}
	cmd.Flags().BoolVar(&fullRebuild, "full-rebuild", false, "Rebuild all cache files from scratch")
	cmd.SetArgs([]string{"--full-rebuild"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "build-cache command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Equal("Building cache...\nExported 42 messages to /tmp/msgvault-analytics\n", stdout.String())
	assert.Equal("Warning: using CSV fallback\n", stderr.String())
}
