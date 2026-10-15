package daemonclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingStatusUsesSharedAPIContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/embeddings/status", r.URL.Path)
		assert.Equal("7", r.URL.Query().Get("source_id"))
		_, _ = w.Write([]byte(`{"generation":{"id":1,"state":"building"},"pending":4,"coverage_sampled_at":"2026-01-01T00:00:00Z","diagnostics_stale":true,"eta_seconds":null,"messages_per_minute":null}`))
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	status, err := client.EmbeddingStatus(context.Background(), 7)
	require.NoError(err)
	assert.Equal(int64(4), status.Pending)
	assert.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), status.CoverageSampledAt)
	assert.True(status.DiagnosticsStale)
	assert.Nil(status.ETASeconds)
}
