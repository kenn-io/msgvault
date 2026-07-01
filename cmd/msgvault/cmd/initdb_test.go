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

func TestInitDBUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/init-db", r.URL.Path, "path")

		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"stats": {
				"total_messages": 3,
				"total_threads": 2,
				"total_accounts": 1,
				"total_labels": 4,
				"total_attachments": 5,
				"database_size_bytes": 1048576
			},
			"notice": "legacy notice\n"
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
	cmd := &cobra.Command{Use: initDBCmd.Use, RunE: initDBCmd.RunE}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "init-db command")
	assert.Equal(int32(1), requests.Load(), "HTTP stats requests")
	assert.Equal("legacy notice\n", stderr.String(), "stderr")
	assert.Equal("Remote: "+server.URL+`
  Messages:    3
  Threads:     2
  Attachments: 5
  Labels:      4
  Sources:     1
  Size:        1.00 MB
`, stdout.String())
}
