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

func TestCacheStatsUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method, "method")
		assert.Equal("/api/v1/cli/cache-stats", r.URL.Path, "path")
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": "ready",
			"total_messages": 42,
			"sources": 3,
			"unique_senders": 9,
			"unique_domains": 4,
			"min_year": 2020,
			"max_year": 2024,
			"total_size_bytes": 10485760,
			"attachment_size_bytes": 2097152,
			"last_sync_at": "2026-06-29T15:30:17Z",
			"last_message_id": 99
		}`))
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

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: cacheStatsCmd.Use, RunE: cacheStatsCmd.RunE}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "cache-stats command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Empty(stderr.String(), "stderr")
	assert.Equal(`Cache Statistics:
  Total messages:    42
  Accounts:          3
  Unique senders:    9
  Unique domains:    4
  Year range:        2020-2024
  Total size:        10.0 MB
  Attachment size:   2.0 MB
  Last sync:         2026-06-29 15:30:17
  Last message ID:   99
`, stdout.String())
}
