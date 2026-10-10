package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMediaSearchMCPDaemonRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/media/search", r.URL.Path)
		if r.URL.Query().Get("limit") == "101" {
			assert.Empty(r.URL.Query().Get("person_id"))
			assert.Equal("from_person", r.URL.Query().Get("direction"))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_media_search","message":"invalid media search"}`))
			return
		}
		assert.Equal("quarterly numbers", r.URL.Query().Get("q"))
		assert.Equal("7", r.URL.Query().Get("person_id"))
		assert.Equal([]string{"from_person"}, r.URL.Query()["direction"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"coverage":{"binding_required":false,"scoped_documents":1,"complete_documents":1,"state":"complete"},"pending_occurrences":0,"unavailable_occurrences":0,"attribution_unavailable":0,"partial":false,"truncated":false,"results":[{"message_id":11,"conversation_id":12,"attachment_id":13,"origin":"supplied","excerpt":"quarterly numbers"}]}`))
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	h := &handlers{mediaSearcher: client}
	response := runTool[generated.MediaSearchResponse](t, ToolSearchMedia, h.searchMedia, map[string]any{"query": "quarterly numbers", "person_id": float64(7), "directions": []any{"from_person"}})
	require.Len(response.Results, 1)
	assert.Equal(int64(11), response.Results[0].MessageID)
	assert.Equal(generated.MediaSearchResultOriginSupplied, response.Results[0].Origin)
	delegated := runToolExpectError(t, ToolSearchMedia, h.searchMedia, map[string]any{"query": "words", "limit": float64(101), "directions": []any{"from_person"}})
	assert.Contains(resultText(t, delegated), "API error (400): invalid media search")
	assert.True(searchMediaDefinition().availability(capabilitiesFor(ServeOptions{MediaSearcher: client})))
	assert.False(searchMediaDefinition().availability(capabilitiesFor(ServeOptions{})))
}
