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

// TestAccountScopesRequireCapableDaemon proves a daemon that predates
// structured account scopes, which would ignore them and widen the result,
// is refused before any request.
func TestAccountScopesRequireCapableDaemon(t *testing.T) {
	scopes := []search.AccountScope{{Unattributed: true}}
	filter := query.MessageFilter{AccountScopes: scopes}
	calls := map[string]func(context.Context, *Client) error{
		"list messages": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).ListMessages(ctx, filter)
			return err
		},
		"aggregate": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).Aggregate(ctx, query.ViewSenders, query.AggregateOptions{AccountScopes: scopes})
			return err
		},
		"deletion by filter": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).GetDeletionTargetsByFilter(ctx, filter)
			return err
		},
		"deletion by search": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).GetDeletionTargetsBySearch(ctx, search.Parse("from:a@example.org"), filter, query.DeletionSearchFast)
			return err
		},
		"deletion by aggregate search": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).GetDeletionTargetsByAggregateSearch(ctx, "from:a@example.org", filter, query.ViewSenders, "a@example.org")
			return err
		},
		"similar": func(ctx context.Context, c *Client) error {
			_, err := c.FindSimilarMessages(ctx, SimilarSearchRequest{MessageID: 1, AccountScopes: scopes})
			return err
		},
	}
	for name, call := range calls {
		for _, version := range []string{"3.1.0", "3.2.0"} {
			t.Run(name+"/"+version, func(t *testing.T) {
				assert := assert.New(t)
				var forwarded atomic.Int32
				var sawScopes atomic.Bool
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/health" {
						writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": version})
						return
					}
					forwarded.Add(1)
					sawScopes.Store(r.URL.Query().Get("account_scopes") == `[{"unattributed":true}]`)
					http.NotFound(w, r)
				}))
				defer server.Close()
				err := call(t.Context(), newTestStore(server, ""))
				if version == "3.1.0" {
					require.Error(t, err)
					assert.Contains(err.Error(), "account filters require daemon API schema 3.2.0")
					assert.Zero(forwarded.Load(), "no non-health request may reach an older daemon")
					return
				}
				assert.Positive(forwarded.Load(), "a capable daemon receives the request")
				assert.True(sawScopes.Load(), "the scopes travel as account_scopes JSON")
			})
		}
	}
}

func TestAccountScopesNeverRideInQueryText(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": "3.2.0"})
	}))
	defer server.Close()
	q := &search.Query{TextTerms: []string{"invoice"}, AccountScopes: []search.AccountScope{{Unattributed: true}}}
	_, err := NewEngineAdapter(newTestStore(server, "")).SearchFast(t.Context(), q, query.MessageFilter{}, 10, 0)
	require.ErrorContains(t, err, "account scopes must travel in the message filter")
}
