package microsoft

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/msgraph"
)

func TestEmailPollGraphAuthenticatedMailboxEvidence(t *testing.T) {
	for _, body := range []string{
		`{"mail":"sample@example.test"}`,
		`{"userPrincipalName":"SAMPLE@example.test"}`,
		`{"mail":"other@example.test","proxyAddresses":["SMTP:sample@example.test"]}`,
		`{"mail":"other@example.test"}`,
		`{}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/me", r.URL.Path)
				assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			client := msgraph.NewClient(server.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
			err := VerifyGraphMailbox(t.Context(), client, "sample@example.test")
			if body == `{"mail":"other@example.test"}` || body == `{}` {
				assert.ErrorIs(t, err, ErrGraphMailboxMismatch)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
