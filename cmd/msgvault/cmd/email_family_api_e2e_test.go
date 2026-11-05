package cmd

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The daemon serves the family through storeAPIAdapter, and the generated
// client's response validation accepts a copy that has no subject, date or
// attributed account. The store owns what the family holds.
func TestEmailFamilyEndpointServesThroughProductionAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "family-owner@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "family-thread", "Family fixture")
	require.NoError(err)
	messageID := persistRelatedMail(t, st, source.ID, conversationID, "family-seed", "family-seed@example.test", "")

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
	client, err := daemonclient.New(daemonclient.Config{URL: httpSrv.URL, AllowInsecure: true, HTTPClient: httpSrv.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })

	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET subject = NULL, sent_at = NULL, account_address = NULL, account_path = NULL WHERE id = ?`), messageID)
	require.NoError(err)
	family, err := client.EmailFamilyContext(t.Context(), messageID)
	require.NoError(err, "the daemon adapter must expose the email-family capability")
	assert.Equal(messageID, family.AnchorMessageID)
	require.Len(family.Members, 1)
	assert.Empty(family.Members[0].Subject)
	assert.Nil(family.Members[0].SentAt)

	_, err = client.EmailFamilyContext(t.Context(), messageID+100)
	require.ErrorIs(err, store.ErrMessageNotFound)
}
