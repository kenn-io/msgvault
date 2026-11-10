package cmd

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"net/mail"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func TestArchiveStoredQuotedMailbox(t *testing.T) {
	parsed, err := mime.Parse([]byte("From: <\"first last\"@example.test>\r\nTo: owner@example.test\r\nSubject: code\r\n\r\nsynthetic\r\n"))
	require.NoError(t, err)
	require.Len(t, parsed.From, 1)
	require.Equal(t, "first last@example.test", parsed.From[0].Email)
	_, senderKey, err := parseStoredMailbox(parsed.From[0].Email)
	require.NoError(t, err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	sender, err := st.EnsureParticipant(parsed.From[0].Email, "Synthetic Sender", "example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "probe", "Synthetic")
	require.NoError(t, err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "gmail-archive", MessageType: "email"}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{parsed.From[0].Email}}}})
	require.NoError(t, err)
	fixture := newArchiveProviderFixture(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = cfg.HomeDir
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionMessageArchive}, Sources: []agentgrant.SourceRef{{Type: "gmail", Identifier: source.Identifier, ArchiveSenderKeys: []string{senderKey}}}}
	archived := true
	result, err := adapter.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived, DryRun: true}, grant, nil)
	require.NoError(t, err, "an allowed sender parsed and stored by production MIME code must remain allowed")
	assert.True(t, result.Archived)
	assert.Zero(t, fixture.writes.Load())
}

func TestArchiveQuotedQuotesDoesNotBypassGrant(t *testing.T) {
	bare := "\"codes\"@example.test"
	header := (&mail.Address{Address: bare}).String()
	parsed, err := mime.Parse([]byte("From: " + header + "\r\nTo: owner@example.test\r\nSubject: code\r\n\r\nsynthetic\r\n"))
	require.NoError(t, err)
	require.Len(t, parsed.From, 1)
	require.Equal(t, bare, parsed.From[0].Email)
	_, _, err = parseStoredMailbox(parsed.From[0].Email)
	require.NoError(t, err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	sender, err := st.EnsureParticipant(parsed.From[0].Email, "Synthetic Sender", "example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "probe", "Synthetic")
	require.NoError(t, err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "gmail-archive", MessageType: "email"}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{parsed.From[0].Email}}}})
	require.NoError(t, err)
	fixture := newArchiveProviderFixture(t)
	cfg := &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "synthetic-owner", AgentAccess: true}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	cfg.Data.DataDir = cfg.HomeDir
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	token, err := owner.IssueAgentTokenWithArchiveSenders(t.Context(), "restricted", []string{"message.archive"}, []int64{source.ID}, nil, map[int64][]string{source.ID: {"codes@example.test"}}, time.Time{})
	require.NoError(t, err)
	delegated, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: token.Secret, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, delegated.Close()) })
	archived := true
	result, err := delegated.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
	var failure *emailtags.MessageTagError
	if assert.ErrorAs(t, err, &failure, "literal quotes in a stored local part must not be removed by authorization") {
		assert.Equal(t, "permission_denied", failure.Code)
	}
	assert.Nil(t, result)
	assert.Zero(t, fixture.writes.Load(), "denied sender must not mutate Gmail")
}
