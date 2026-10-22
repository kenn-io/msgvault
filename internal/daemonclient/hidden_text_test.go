package daemonclient

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIHybridSearchHiddenTextPresent(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/search":
			assert.Equal("hybrid", r.URL.Query().Get("mode"))
			_, _ = w.Write([]byte(`{"generation":{"id":1,"state":"active"},"results":[{"id":1,"sent_at":"2026-01-01T00:00:00Z","hidden_text_present":true},{"id":2,"sent_at":"2026-01-01T00:00:00Z","hidden_text_present":false}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	result, err := client.GetCLIHybridSearch(t.Context(), CLIHybridSearchRequest{Query: "needle", Mode: "hybrid"})
	require.NoError(err)
	require.Len(result.Results, 2)
	for i, want := range []bool{true, false} {
		data, err := json.Marshal(result.Results[i].Message)
		require.NoError(err)
		var row map[string]any
		require.NoError(json.Unmarshal(data, &row))
		assert.Equal(want, row["hidden_text_present"])
	}
}
