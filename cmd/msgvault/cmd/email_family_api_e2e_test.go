package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEmailFamilyEndpointServesThroughProductionAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "family-owner@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "family-thread", "Family fixture")
	require.NoError(err)
	messageID, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "family-seed", MessageType: "email"},
		RawMIME: []byte("Message-ID: <family-seed@example.test>\r\n\r\n"),
	})
	require.NoError(err)

	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	resp, err := http.Get(httpSrv.URL + fmt.Sprintf("/api/v1/messages/%d/family", messageID))
	require.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(http.StatusOK, resp.StatusCode, "the daemon adapter must expose the email-family capability")

	var family store.EmailFamily
	require.NoError(json.NewDecoder(resp.Body).Decode(&family))
	assert.Equal(messageID, family.AnchorMessageID)
	require.Len(family.Members, 1)
	assert.Equal(messageID, family.Members[0].MessageID)
}
