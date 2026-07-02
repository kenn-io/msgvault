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

func TestSyncUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/sync", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("email"), "email query")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Starting incremental sync for alice@example.com\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"sync warning\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	configureRemoteSyncTest(t, server.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: syncIncrementalCmd.Use, Args: syncIncrementalCmd.Args, RunE: syncIncrementalCmd.RunE}
	cmd.SetArgs([]string{"alice@example.com"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "sync command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Equal("Starting incremental sync for alice@example.com\n", stdout.String())
	assert.Equal("sync warning\n", stderr.String())
}

func TestSyncFullUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/sync-full", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("email"), "email query")
		assert.Equal("from:bob@example.com", r.URL.Query().Get("query"), "query flag")
		assert.Equal("2024-01-01", r.URL.Query().Get("after"), "after flag")
		assert.Equal("2024-12-31", r.URL.Query().Get("before"), "before flag")
		assert.Equal("25", r.URL.Query().Get("limit"), "limit flag")
		assert.Equal("true", r.URL.Query().Get("noresume"), "noresume flag")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Starting full sync for alice@example.com\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"sync-full warning\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	configureRemoteSyncTest(t, server.URL)
	resetSyncFullFlagsForTest(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: syncFullCmd.Use, Args: syncFullCmd.Args, RunE: syncFullCmd.RunE}
	cmd.Flags().StringVar(&syncQuery, "query", "", "Gmail search query")
	cmd.Flags().BoolVar(&syncNoResume, "noresume", false, "Force fresh sync")
	cmd.Flags().StringVar(&syncBefore, "before", "", "Only messages before this date")
	cmd.Flags().StringVar(&syncAfter, "after", "", "Only messages after this date")
	cmd.Flags().IntVar(&syncLimit, "limit", 0, "Limit number of messages")
	cmd.SetArgs([]string{
		"alice@example.com",
		"--query", "from:bob@example.com",
		"--after", "2024-01-01",
		"--before", "2024-12-31",
		"--limit", "25",
		"--noresume",
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "sync-full command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Equal("Starting full sync for alice@example.com\n", stdout.String())
	assert.Equal("sync-full warning\n", stderr.String())
}

func configureRemoteSyncTest(t *testing.T, remoteURL string) {
	t.Helper()

	dataDir := t.TempDir()
	withStoreResolverConfig(t, &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote: config.RemoteConfig{
			URL:           remoteURL,
			AllowInsecure: true,
		},
	})
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	t.Cleanup(func() { logger = oldLogger })
}

func resetSyncFullFlagsForTest(t *testing.T) {
	t.Helper()

	oldQuery := syncQuery
	oldNoResume := syncNoResume
	oldBefore := syncBefore
	oldAfter := syncAfter
	oldLimit := syncLimit
	syncQuery = ""
	syncNoResume = false
	syncBefore = ""
	syncAfter = ""
	syncLimit = 0
	t.Cleanup(func() {
		syncQuery = oldQuery
		syncNoResume = oldNoResume
		syncBefore = oldBefore
		syncAfter = oldAfter
		syncLimit = oldLimit
	})
}
