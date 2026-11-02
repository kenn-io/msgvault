package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/query"
)

func TestDaemonArchiveSQLUsesRestrictedEngine(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.AutoBuildCache = false
	_, err := s.DB().Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'user@example.com');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
			VALUES (1, 1, 'thread-1', 'email_thread');
		INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, sent_at)
			VALUES (1, 1, 'message-1', 1, 'email', '2024-01-01 00:00:00');
	`)
	requirements.NoError(err)
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	ownerEngine, err := openDaemonDuckDBEngine(c, s)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(ownerEngine.Close()) })
	jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error { return nil })

	result, accepted, err := runDaemonSQLQueryWithJobs(t.Context(), c, s, ownerEngine,
		"SELECT COUNT(*) FROM messages", daemonSQLQueryOptions{archiveOnly: true}, jobs)
	requirements.NoError(err)
	assertions.Nil(accepted)
	requirements.Len(result.Rows, 1)
	assertions.EqualValues(1, result.Rows[0][0])

	outside := filepath.Join(t.TempDir(), "outside.txt")
	requirements.NoError(os.WriteFile(outside, []byte("synthetic outside content"), 0o600))
	sql := "SELECT content FROM read_text('" + strings.ReplaceAll(filepath.ToSlash(outside), "'", "''") + "')"
	_, _, err = runDaemonSQLQueryWithJobs(t.Context(), c, s, ownerEngine,
		sql, daemonSQLQueryOptions{archiveOnly: true}, jobs)
	requirements.Error(err)
	assertions.Contains(err.Error(), "Permission Error")

	result, _, err = runDaemonSQLQueryWithJobs(t.Context(), c, s, ownerEngine,
		sql, daemonSQLQueryOptions{}, jobs)
	requirements.NoError(err)
	assertions.Equal("synthetic outside content", result.Rows[0][0])
}

func TestDaemonArchiveSQLRejectsLivePolicyDrift(t *testing.T) {
	requirements := require.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.AutoBuildCache = false
	_, err := s.DB().Exec(`INSERT INTO sources (id,source_type,identifier) VALUES (1,'beeper','cache@example.com'); INSERT INTO conversations (id,source_id,source_conversation_id,conversation_type) VALUES (1,1,'room','group'); INSERT INTO messages (id,source_id,source_message_id,conversation_id,message_type,sent_at) VALUES (1,1,'m1',1,'chat','2024-01-01 00:00:00')`)
	requirements.NoError(err)
	_, err = buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)
	e, err := openDaemonDuckDBEngine(c, s)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error { return nil })
	_, _, err = runDaemonSQLQueryWithJobs(t.Context(), c, s, e, "SELECT COUNT(*) FROM messages", daemonSQLQueryOptions{archiveOnly: true}, jobs)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), 1, "room", conversationpolicy.Cold))
	_, err = e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	requirements.ErrorIs(err, query.ErrCacheUnavailable)
	_, _, err = runDaemonSQLQueryWithJobs(t.Context(), c, s, e, "SELECT COUNT(*) FROM messages", daemonSQLQueryOptions{archiveOnly: true}, jobs)
	requirements.ErrorIs(err, api.ErrSQLQueryEngineUnavailable)
}
