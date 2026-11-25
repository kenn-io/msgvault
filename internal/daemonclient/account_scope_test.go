package daemonclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

func TestPipeORSearchTransport(t *testing.T) {
	var requested atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": "3.19.0"})
			return
		}
		requested.Store(true)
		assert.Equal(t, "/api/v1/search/deep", r.URL.Path)
		assert.Equal(t, "alpha | gamma", r.URL.Query().Get("q"))
		writeJSONResponse(t, w, map[string]any{"messages": []any{}, "total_count": 0, "stats": map[string]any{}})
	}))
	defer server.Close()
	engine := NewEngineAdapter(newTestStore(server, ""))
	_, err := engine.Search(t.Context(), search.ParseOR("alpha | gamma"), 10, 0)
	require.NoError(t, err)
	assert.True(t, requested.Load())
}

func TestAccountScopesNeverRideInQueryText(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": "3.11.0"})
	}))
	defer server.Close()
	q := &search.Query{TextTerms: []string{"invoice"}, AccountScopes: []search.AccountScope{{Unattributed: true}}}
	_, err := NewEngineAdapter(newTestStore(server, "")).GetDeletionTargetsBySearch(t.Context(), q, query.MessageFilter{}, query.DeletionSearchFast)
	require.ErrorContains(t, err, "account scopes must travel in the message filter")
}

// Query scopes ride the filter's account_scopes parameter on search paths
// that otherwise send only query text.
func TestQueryAccountScopesTravelAsParameters(t *testing.T) {
	scopes := []search.AccountScope{{Unattributed: true}}
	calls := map[string]func(context.Context, *Client) error{
		"search": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).Search(ctx, &search.Query{TextTerms: []string{"invoice"}, AccountScopes: scopes}, 10, 0)
			return err
		},
		"body search": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).SearchMessageBodies(ctx, &search.Query{TextTerms: []string{"invoice"}, AccountScopes: scopes}, 10, 0)
			return err
		},
		"fast search": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).SearchFast(ctx, &search.Query{TextTerms: []string{"invoice"}, AccountScopes: scopes}, query.MessageFilter{}, 10, 0)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			var sawScopes atomic.Bool
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": "3.11.0"})
					return
				}
				sawScopes.Store(r.URL.Query().Get("account_scopes") == `[{"unattributed":true}]`)
				http.NotFound(w, r)
			}))
			defer server.Close()
			_ = call(t.Context(), newTestStore(server, ""))
			assert.True(t, sawScopes.Load(), "the scopes travel as account_scopes JSON")
		})
	}
}
