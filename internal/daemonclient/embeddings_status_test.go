package daemonclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingStatusUsesSharedAPIContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/embeddings/status", r.URL.Path)
		assert.Equal("7", r.URL.Query().Get("source_id"))
		_, _ = w.Write([]byte(`{"generation":{"id":1,"state":"building"},"pending":4,"eta_seconds":null,"messages_per_minute":null}`))
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	status, err := client.EmbeddingStatus(context.Background(), 7)
	require.NoError(err)
	assert.Equal(int64(4), status.Pending)
	assert.Nil(status.ETASeconds)
}
