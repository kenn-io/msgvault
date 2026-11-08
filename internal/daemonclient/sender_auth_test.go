package daemonclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
)

func TestRemoteMessageReadsKeepSenderAuth(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		read       func(context.Context, *Client) (*query.MessageDetail, error)
	}{
		{"detail", "/api/v1/messages/42", func(ctx context.Context, c *Client) (*query.MessageDetail, error) {
			return NewEngineAdapter(c).GetMessage(ctx, 42)
		}},
		{"CLI", "/api/v1/cli/message", func(ctx context.Context, c *Client) (*query.MessageDetail, error) { return c.GetCLIMessage(ctx, "42") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.path, r.URL.Path)
				from := any("sender@example.test")
				if tc.name == "CLI" {
					from = []any{map[string]any{"email": "sender@example.test"}}
				}
				writeJSONResponse(t, w, map[string]any{"id": 42, "sent_at": "2026-11-03T12:00:00Z", "from": from, "sender_auth": map[string]any{"verified": true, "domain": "example.test", "authserv_id": "mx.google.com", "spf": "pass", "dkim": "policy", "dmarc": "pass"}})
			})
			got, err := tc.read(t.Context(), client)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, &senderauth.Verdict{Verified: true, Domain: "example.test", AuthservID: "mx.google.com", SPF: "pass", DKIM: "policy", DMARC: "pass"}, got.SenderAuth)
		})
	}
}
