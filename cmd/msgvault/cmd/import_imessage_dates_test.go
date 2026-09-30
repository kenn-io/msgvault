package cmd

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/store"
)

// Exercise the reported path: real chat.db import followed by a full cache build.
func TestBuildCacheAfterIMessageSentinelDates(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	tmp := t.TempDir()
	chatPath := filepath.Join(tmp, "chat.db")
	chat, err := sql.Open("sqlite3", chatPath+"?_journal_mode=WAL")
	require.NoError(err)
	t.Cleanup(func() { _ = chat.Close() })
	_, err = chat.Exec(`
 CREATE TABLE message (guid TEXT, text TEXT, attributedBody BLOB, date INTEGER,
  is_from_me INTEGER, service TEXT, cache_has_attachments INTEGER, handle_id INTEGER);
 CREATE TABLE handle (id TEXT);
 CREATE TABLE chat (guid TEXT, display_name TEXT, chat_identifier TEXT);
 CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
 CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
 INSERT INTO handle VALUES ('peer@example.test');
 INSERT INTO chat VALUES ('any;-;synthetic', NULL, 'peer@example.test');
 INSERT INTO chat_handle_join VALUES (1,1);
 `)
	require.NoError(err)
	for i, date := range []int64{math.MinInt64, math.MaxInt64, 725760000000000000, 725846400000000000} {
		var body any
		if i >= 2 {
			body = "Synthetic message"
		}
		_, err = chat.Exec(`INSERT INTO message VALUES ('synthetic', ?, NULL, ?, ?, 'SMS', 0, 1)`, body, date, i%2)
		require.NoError(err)
		_, err = chat.Exec(`INSERT INTO chat_message_join VALUES (1,?)`, i+1)
		require.NoError(err)
	}
	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	src, err := resolveImessageSource(st)
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "owner@example.test", "manual"))
	c, err := imessage.NewClient(chatPath, imessage.WithOwnerHandle("owner@example.test"))
	require.NoError(err)
	t.Cleanup(func() { _ = c.Close() })
	summary, err := c.Import(context.Background(), st, src.ID)
	require.NoError(err)
	assert.Equal(4, summary.MessagesImported)
	assert.Zero(summary.Skipped)
	var total, undated int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&total))
	assert.Equal(4, total)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE sent_at IS NULL AND internal_date IS NULL`).Scan(&undated))
	assert.Equal(2, undated)
	require.NoError(st.Close())
	analyticsDir := filepath.Join(tmp, "analytics")
	result, err := buildCache(dbPath, analyticsDir, true)
	require.NoError(err, "sentinel placeholders must not break identity validation")
	assert.False(result.Skipped)

	// Undated rows stay archived; the dated analytics contain only real dates.
	analytics, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = analytics.Close() })
	var exported int
	require.NoError(analytics.QueryRow(`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)`,
		filepath.Join(analyticsDir, "messages", "**", "*.parquet")).Scan(&exported))
	assert.Equal(2, exported)
}

func TestReimportClearedIMessageDateRebuildsExistingCache(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	tmp := t.TempDir()
	chatPath := filepath.Join(tmp, "chat.db")
	chat, err := sql.Open("sqlite3", chatPath+"?_journal_mode=WAL")
	require.NoError(err)
	t.Cleanup(func() { _ = chat.Close() })
	_, err = chat.Exec(`
 CREATE TABLE message (guid TEXT, text TEXT, attributedBody BLOB, date INTEGER,
  is_from_me INTEGER, service TEXT, cache_has_attachments INTEGER, handle_id INTEGER);
 CREATE TABLE handle (id TEXT);
 CREATE TABLE chat (guid TEXT, display_name TEXT, chat_identifier TEXT);
 CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
 CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
 INSERT INTO handle VALUES ('peer@example.test');
 INSERT INTO chat VALUES ('any;-;synthetic', NULL, 'peer@example.test');
 INSERT INTO message VALUES ('synthetic-1', NULL, NULL, 725760000000000000, 0, 'SMS', 0, NULL);
 INSERT INTO message VALUES ('synthetic-2', NULL, NULL, 725846400000000000, 0, 'SMS', 0, NULL);
 INSERT INTO chat_message_join VALUES (1,1);
 INSERT INTO chat_message_join VALUES (1,2);
 `)
	require.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = tmp
	dbPath := cfg.DatabaseDSN()
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	src, err := resolveImessageSource(st)
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "owner@example.test", "manual"))
	c, err := imessage.NewClient(chatPath)
	require.NoError(err)
	t.Cleanup(func() { _ = c.Close() })
	summary, err := c.Import(context.Background(), st, src.ID)
	require.NoError(err)
	assert.Equal(2, summary.MessagesImported)
	assert.Zero(summary.Skipped)
	_ = retitleImessageChats(st)
	require.NoError(st.Close())
	result, err := buildCache(dbPath, cfg.AnalyticsDir(), true)
	require.NoError(err)
	assert.False(result.Skipped)

	cachedMessages := func(sourceMessageID string) int {
		analytics, err := sql.Open("duckdb", "")
		require.NoError(err)
		defer func() { _ = analytics.Close() }()
		var count int
		require.NoError(analytics.QueryRow(`
 SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)
 WHERE source_message_id = ?`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet"), sourceMessageID).Scan(&count))
		return count
	}
	assert.Equal(1, cachedMessages("1"), "initial cache should contain the dated message")

	st, err = store.Open(dbPath)
	require.NoError(err)
	_, err = chat.Exec(`UPDATE message SET date = ? WHERE ROWID = 1`, int64(math.MaxInt64))
	require.NoError(err)
	summary, err = c.Import(context.Background(), st, src.ID)
	require.NoError(err)
	assert.Equal(2, summary.MessagesImported)
	var sentAt, internalDate sql.NullTime
	require.NoError(st.DB().QueryRow(`
 SELECT sent_at, internal_date FROM messages WHERE source_id = ? AND source_message_id = '1'`, src.ID).Scan(&sentAt, &internalDate))
	assert.False(sentAt.Valid)
	assert.False(internalDate.Valid)

	state := &invocation{cfg: cfg}
	require.Equal(1, summary.DatesCleared)
	require.NoError(finishImessageImport(st, state, summary))
	assert.Equal(0, cachedMessages("1"), "the cache must drop the message whose date was cleared")
}

func TestFinishImessageImportSkipsCacheBuildForPostgres(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	src, err := resolveImessageSource(st)
	require.NoError(err)
	participantID, err := st.EnsureParticipant("peer@example.test", "Synthetic Person", "example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		src.ID, "synthetic-chat", "direct_chat", "peer@example.test",
	)
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipant(conversationID, participantID, "member"))

	cfg := config.NewDefaultConfig()
	cfg.Data.DatabaseURL = "postgres://user:pass@example.test:5432/msgvault"
	state := &invocation{cfg: cfg}
	require.NoError(finishImessageImport(st, state, &imessage.ImportSummary{}))
	var title string
	require.NoError(st.DB().QueryRow(`SELECT title FROM conversations WHERE id = ?`, conversationID).Scan(&title))
	assert.Equal("Synthetic Person", title)
}

func TestPrintImessageSummaryReportsClearedDates(t *testing.T) {
	assert := assert.New(t)
	done := captureStdout(t)
	printImessageSummary(&imessage.ImportSummary{DatesCleared: 2}, time.Now())
	assert.Contains(done(), "  Dates cleared:    2\n")

	done = captureStdout(t)
	printImessageSummary(&imessage.ImportSummary{}, time.Now())
	assert.NotContains(done(), "Dates cleared")
}
