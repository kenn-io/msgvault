package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
)

func TestMCPCommandUsesDaemonInsteadOfOpeningLocalDatabase(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{
			DataDir: filepath.Join(t.TempDir(), "missing-parent", "data"),
		},
		Remote: config.RemoteConfig{
			URL:           "http://daemonclient.example:8080",
			AllowInsecure: true,
		},
	})

	savedHTTPAddr := mcpHTTPAddr
	savedAllowInsecure := mcpHTTPAllowInsecure
	mcpHTTPAddr = "127.0.0.1:0"
	mcpHTTPAllowInsecure = false
	t.Cleanup(func() {
		mcpHTTPAddr = savedHTTPAddr
		mcpHTTPAllowInsecure = savedAllowInsecure
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cmd := mcpCmd
	cmd.SetContext(ctx)
	err := cmd.RunE(cmd, nil)

	require.Error(err, "canceled MCP serve should return")
	require.ErrorIs(err, context.Canceled, "error should preserve context cancellation: %v", err)
	assert.NotContains(err.Error(), "open database", "MCP command must not open SQLite directly")
}

func TestDaemonMCPServeOptionsDisablesVectorToolsWhenDaemonVectorUnavailable(t *testing.T) {
	withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})
	client := newMCPStatsDaemonClient(t, `{
		"total_messages": 0,
		"total_threads": 0,
		"total_accounts": 0,
		"total_labels": 0,
		"total_attachments": 0,
		"database_size_bytes": 0
	}`)

	opts, err := daemonMCPServeOptions(context.Background(), client)
	require.NoError(t, err)

	assert.NotNil(t, opts.Engine, "engine")
	assert.NotNil(t, opts.AttachmentReader, "attachment reader")
	assert.NotNil(t, opts.ManifestSaver, "manifest saver")
	assert.Nil(t, opts.HybridSearcher, "hybrid searcher")
	assert.Nil(t, opts.SimilarSearcher, "similar searcher")
}

func TestDaemonMCPServeOptionsSavesDeletionManifestsThroughDaemon(t *testing.T) {
	withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})

	var manifestRequests atomic.Int32
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/stats":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"total_messages": 0,
				"total_threads": 0,
				"total_accounts": 0,
				"total_labels": 0,
				"total_attachments": 0,
				"database_size_bytes": 0
			}`))
		case "/api/v1/cli/deletion-manifests":
			manifestRequests.Add(1)
			assert.Equal(t, http.MethodPost, r.Method, "method")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"batch-1","message_count":1}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	opts, err := daemonMCPServeOptions(context.Background(), client)
	require.NoError(t, err)
	require.NotNil(t, opts.ManifestSaver, "manifest saver")

	manifest := deletion.NewManifest("mcp test", []string{"gmail-001"})
	err = opts.ManifestSaver.SaveManifest(context.Background(), manifest)
	require.NoError(t, err)
	assert.Equal(t, int32(1), manifestRequests.Load(), "manifest requests")
}

func TestDaemonMCPServeOptionsEnablesVectorToolsWhenDaemonVectorAvailable(t *testing.T) {
	withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})
	client := newMCPStatsDaemonClient(t, `{
		"total_messages": 0,
		"total_threads": 0,
		"total_accounts": 0,
		"total_labels": 0,
		"total_attachments": 0,
		"database_size_bytes": 0,
		"vector_search": {
			"enabled": true,
			"active_generation": {
				"id": 1,
				"model": "text-embedding-3-small",
				"dimension": 1536,
				"fingerprint": "text-embedding-3-small:1536",
				"state": "active",
				"message_count": 10
			},
			"missing_embeddings_total": 0
		}
	}`)

	opts, err := daemonMCPServeOptions(context.Background(), client)
	require.NoError(t, err)

	assert.NotNil(t, opts.HybridSearcher, "hybrid searcher")
	assert.NotNil(t, opts.SimilarSearcher, "similar searcher")
}

func newMCPStatsDaemonClient(t *testing.T, statsJSON string) *daemonclient.Client {
	t.Helper()

	return newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats", r.URL.Path, "path")
		assert.Equal(t, "key", r.Header.Get("X-Api-Key"), "api key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statsJSON))
	})
}

func newMCPDaemonClient(t *testing.T, handler http.HandlerFunc) *daemonclient.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "key", r.Header.Get("X-Api-Key"), "api key")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client, err := daemonclient.New(daemonclient.Config{
		URL:           srv.URL,
		APIKey:        "key",
		AllowInsecure: true,
	})
	require.NoError(t, err)
	return client
}

func TestNormalizeMCPHTTPAddr(t *testing.T) {
	t.Run("bare_port_defaults_to_loopback", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr("8080", false)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:8080", got)
	})

	t.Run("colon_port_defaults_to_loopback", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr(":8080", false)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:8080", got)
	})

	t.Run("explicit_loopback_passes", func(t *testing.T) {
		cases := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"}
		for _, c := range cases {
			got, err := normalizeMCPHTTPAddr(c, false)
			require.NoError(t, err, "%s", c)
			assert.Equal(t, c, got, "%s: should be unchanged", c)
		}
	})

	t.Run("non_loopback_rejected_without_optin", func(t *testing.T) {
		cases := []string{
			"0.0.0.0:8080",
			"192.168.1.5:8080",
			"vault.local:8080",
			// Regression: empty-bracket host parses cleanly via
			// net.SplitHostPort but binds to all interfaces. Must
			// be rejected, not silently treated as loopback.
			"[]:8080",
		}
		for _, c := range cases {
			_, err := normalizeMCPHTTPAddr(c, false)
			require.Error(t, err, "%s: expected refusal", c)
			assert.ErrorContains(t, err, "--http-allow-insecure", "%s: expected hint", c)
		}
	})

	t.Run("non_loopback_allowed_with_optin", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr("0.0.0.0:8080", true)
		require.NoError(t, err)
		require.Equal(t, "0.0.0.0:8080", got)
	})

	t.Run("empty_rejected", func(t *testing.T) {
		_, err := normalizeMCPHTTPAddr("", false)
		require.Error(t, err, "expected error for empty addr")
	})

	t.Run("garbage_rejected", func(t *testing.T) {
		_, err := normalizeMCPHTTPAddr("not-a-port", false)
		require.Error(t, err, "expected error for non-port, non-host:port")
	})
}
