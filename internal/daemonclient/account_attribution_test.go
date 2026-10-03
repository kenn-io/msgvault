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

// TestAccountOperatorsRequireCapableDaemon proves an older daemon, which would
// read received: as free text, is refused before any request that could
// search or resolve deletion targets.
func TestAccountOperatorsRequireCapableDaemon(t *testing.T) {
	const text = "invoice received:work@example.org"
	calls := map[string]func(context.Context, *Client) error{
		"cli search": func(ctx context.Context, c *Client) error {
			_, err := c.GetCLISearch(ctx, CLISearchRequest{Query: text})
			return err
		},
		"search fast": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).SearchFast(ctx, search.Parse(text), query.MessageFilter{}, 10, 0)
			return err
		},
		"deletion by search": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).GetDeletionTargetsBySearch(ctx, search.Parse(text), query.MessageFilter{}, query.DeletionSearchFast)
			return err
		},
		"deletion by aggregate search": func(ctx context.Context, c *Client) error {
			_, err := NewEngineAdapter(c).GetDeletionTargetsByAggregateSearch(ctx, text, query.MessageFilter{}, query.ViewSenders, "sender@example.org")
			return err
		},
	}
	for name, call := range calls {
		for _, version := range []string{"3.0.0", "3.1.0"} {
			t.Run(name+"/"+version, func(t *testing.T) {
				assert := assert.New(t)
				var forwarded atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/health" {
						writeJSONResponse(t, w, map[string]any{"status": "ok", "api_schema_version": version})
						return
					}
					forwarded.Add(1)
					http.NotFound(w, r)
				}))
				defer server.Close()
				err := call(t.Context(), newTestStore(server, ""))
				if version == "3.0.0" {
					require.Error(t, err)
					assert.Contains(err.Error(), "account: and received: filters require daemon API schema 3.1.0")
					assert.Zero(forwarded.Load(), "no non-health request may reach an older daemon")
					return
				}
				if err != nil {
					assert.NotContains(err.Error(), "require daemon API schema")
				}
				assert.Positive(forwarded.Load(), "a capable daemon receives the request")
			})
		}
	}
}
