package cmd

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

// Failed writes disclose label snapshots only to callers with read authority.
func TestArchiveOnlyGrantFailureBodyHidesLabels(t *testing.T) {
	for _, caller := range []string{"archive-only", "archive-reader", "owner"} {
		t.Run(caller, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "account@example.test")
			require.NoError(t, err)
			sender, err := st.EnsureParticipant("codes@example.test", "Code Sender", "example.test")
			require.NoError(t, err)
			conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-archive", "Archive")
			require.NoError(t, err)
			id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "gmail-archive", MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}}, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{"codes@example.test"}, DisplayNames: []string{"Code Sender"}}}})
			require.NoError(t, err)
			cfg := &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
			cfg.Data.DataDir = cfg.HomeDir
			cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "no-credentials.json")
			provider := newArchiveProviderFixture(t)
			adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), emailTagClientFactory: func(context.Context, *store.Source) (gmail.API, error) {
				return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-fixture-token"}), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
			}, draftCacheRefresh: func(context.Context, string) error { return nil }}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
			server := httptest.NewServer(srv.Router())
			t.Cleanup(server.Close)
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { _ = owner.Close() })
			permissions := []string{"message.archive"}
			if caller == "archive-reader" {
				permissions = append(permissions, "message.read")
			}
			token, err := owner.IssueAgentTokenWithArchiveSenders(t.Context(), caller, permissions, []int64{source.ID}, nil, nil, time.Time{})
			require.NoError(t, err)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: token.Secret, AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			provider.failReadback.Store(true)
			archived := true
			requestClient := client
			if caller == "owner" {
				requestClient = owner
			}
			_, err = requestClient.ArchiveMessage(t.Context(), id, emailtags.ArchiveChange{Archived: &archived})
			var failure *emailtags.MessageTagError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, "remote_unknown", failure.Code)
			if caller == "archive-only" {
				assert.Nil(t, failure.Result, "archive-only grant received message label state: %+v", failure.Result)
			} else {
				require.NotNil(t, failure.Result)
				assert.Contains(t, failure.Result.Tags, "Label_keep")
				assert.Contains(t, failure.Result.Before, "INBOX")
			}
			assert.Equal(t, int64(1), provider.writes.Load())
		})
	}
}
