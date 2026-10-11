package cmd

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The daemon serves accounts through storeAPIAdapter, so the pickers only see
// virtual accounts when the adapter forwards them.
func TestStoreAPIAdapterListsVirtualAccounts(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("mbox", "archive-1")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
	conv, err := st.EnsureConversation(src.ID, "thread", "")
	require.NoError(err)
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "work", MessageType: "email"},
		RawMIME: []byte("X-Delivered-To: work@example.org\r\n\r\nbody"),
	})
	require.NoError(err)

	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})
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
	require.Contains(body.Accounts[0].VirtualAccounts, store.VirtualAccount{
		Key: store.VirtualIdentityKey(src.ID, "work@example.org"), SourceID: src.ID, AccountAddress: "work@example.org", MessageCount: 1,
	})
}
