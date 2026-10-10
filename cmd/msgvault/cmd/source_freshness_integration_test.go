package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
)

func TestCommittedMessageDetailsCanLeadPublishedAnalyticsSearch(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	cfg, st := openTestDaemonAnalyticsStore(t)
	cfg.Analytics.AutoBuildCache = false
	_, err := st.DB().Exec(`
 INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'sender@example.com');
 INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
 VALUES (1, 1, 'synthetic-thread', 'email_thread');
 INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, subject, sent_at)
 VALUES (1, 1, 'synthetic-old', 1, 'email', 'Earlier item', '2024-01-01 00:00:00');`)
	requirements.NoError(err)
	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	requirements.NoError(err)
	engine, err := openDaemonDuckDBEngine(cfg, st)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error {
		_, buildErr := buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
		return buildErr
	})
	fixture := api.NewServerWithOptions(api.ServerOptions{
		Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
		SQLQueryRunner: func(ctx context.Context, sql string, fresh bool) (*query.QueryResult, *api.CacheBuildAccepted, error) {
			return runDaemonSQLQueryWithJobs(ctx, cfg, st, engine, sql, daemonSQLQueryOptions{fresh: fresh}, jobs)
		},
	})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	searchSQL := "SELECT id FROM messages WHERE subject = 'New synthetic item'"
	before, err := client.RunSQLQuery(t.Context(), searchSQL)
	requirements.NoError(err)
	requirements.NotNil(before.Cache)
	requirements.NotEmpty(before.Cache.Generation)
	requirements.Empty(before.Rows)
	_, err = st.DB().Exec(`INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, subject, sent_at)
 VALUES (2, 1, 'synthetic-new', 1, 'email', 'New synthetic item', '2024-01-02 00:00:00')`)
	requirements.NoError(err)
	details, err := client.GetMessageContext(t.Context(), 2)
	requirements.NoError(err)
	requirements.NotNil(details)
	assertions.Equal("New synthetic item", details.Subject)
	stale, err := client.RunSQLQuery(t.Context(), searchSQL)
	requirements.NoError(err)
	requirements.NotNil(stale.Cache)
	assertions.Empty(stale.Rows)
	assertions.Equal(before.Cache.Generation, stale.Cache.Generation)
	assertions.NotEmpty(stale.Cache.PublishedAt)
	assertions.NotEmpty(stale.Cache.StaleReason)
	assertions.EqualValues(1, stale.Cache.PendingAdditions)
	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	requirements.NoError(err)
	published, err := client.RunSQLQuery(t.Context(), searchSQL)
	requirements.NoError(err)
	requirements.Len(published.Rows, 1)
	rawID, ok := published.Rows[0][0].(jsontext.Value)
	requirements.True(ok)
	var messageID int64
	requirements.NoError(json.Unmarshal(rawID, &messageID))
	assertions.Equal(int64(2), messageID)
	requirements.NotNil(published.Cache)
	assertions.NotEqual(stale.Cache.Generation, published.Cache.Generation)
	assertions.Zero(published.Cache.PendingAdditions)
	assertions.Empty(published.Cache.StaleReason)
}
