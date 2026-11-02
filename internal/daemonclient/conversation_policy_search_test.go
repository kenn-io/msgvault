package daemonclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

func TestConversationPolicyIncludeColdRemoteRoundTrip(t *testing.T) {
	calls := 0
	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": "3.19.0"})
			return
		}
		calls++
		q := search.ParseOR(r.URL.Query().Get("q"))
		assert.True(t, q.IncludeCold)
		assert.Len(t, q.Alternatives, 2)
		writeJSONResponse(t, w, map[string]any{"query": r.URL.Query().Get("q"), "count": 0, "has_more": false, "messages": []any{}})
	})
	_, err := NewEngineAdapter(s).Search(context.Background(), search.ParseOR("subject:needle | subject:other include:cold"), 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestConversationPolicyIncludeColdNeverReranksCold(t *testing.T) {
	calls := 0
	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
	e := NewEngineAdapter(s)
	_, err := e.SearchSemanticMessages(context.Background(), query.SemanticMessageSearchRequest{Query: "needle include:cold"})
	require.ErrorContains(t, err, "lexical")
	assert.Zero(t, calls)
}
