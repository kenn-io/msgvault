package cmd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestStoreAPIAdapterRefreshesAccountCatalogAfterIdentityChange runs the
// account catalog through the production store adapter: confirming an
// address after the catalog is warm must show it at once rather than serve
// the earlier read as current.
func TestStoreAPIAdapterRefreshesAccountCatalogAfterIdentityChange(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
	})
	src, err := st.GetOrCreateSource("mbox", "archive@example.net")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "thread", "Thread")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv, SourceMessageID: "m1", MessageType: "email",
	})
	require.NoError(err)

	addresses := func() []string {
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil))
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var body struct {
			Accounts []struct {
				VirtualAccounts []store.VirtualAccount `json:"virtual_accounts"`
			} `json:"accounts"`
		}
		require.NoError(json.NewDecoder(response.Body).Decode(&body))
		require.Len(body.Accounts, 1)
		var out []string
		for _, child := range body.Accounts[0].VirtualAccounts {
			if !child.Unattributed {
				out = append(out, child.AccountAddress)
			}
		}
		return out
	}
	require.Empty(addresses(), "the catalog is warm and lists no confirmed address")
	require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
	assert.Equal(t, []string{"work@example.org"}, addresses())
}
