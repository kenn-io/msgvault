package cmd

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// TestScheduledIMessageReimportRebuildsCacheAfterDateClear covers the daemon
// path of TestReimportClearedIMessageDateRebuildsExistingCache: a scheduled
// import that clears an archived date must not leave the old row in the
// Parquet cache, even though the cache was published moments ago (inside the
// rebuild throttle) and no message ID changed.
func TestScheduledIMessageReimportRebuildsCacheAfterDateClear(t *testing.T) {
	scheduledIMessageDateClear(t, false)
}

// TestScheduledIMessageFailedImportInvalidatesCacheAfterDateClear covers an
// import that clears a date and then fails: the cache must still stop serving
// the cleared row, and the import error must be preserved.
func TestScheduledIMessageFailedImportInvalidatesCacheAfterDateClear(t *testing.T) {
	scheduledIMessageDateClear(t, true)
}

func scheduledIMessageDateClear(t *testing.T, failAfterClear bool) {
	t.Helper()
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
	cfg.Analytics.AutoBuildCache = true
	cfg.Analytics.MinRebuildInterval = 6 * time.Hour
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
	_, err = c.Import(context.Background(), st, src.ID)
	require.NoError(err)
	_, err = buildCache(dbPath, cfg.AnalyticsDir(), true)
	require.NoError(err)

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
	require.Equal(1, cachedMessages("1"), "initial cache should contain the dated message")

	_, err = chat.Exec(`UPDATE message SET date = ? WHERE ROWID = 1`, int64(math.MaxInt64))
	require.NoError(err)

	// Stand in for the build subprocess with the real full build.
	builds := 0
	oldRunBuild := runScheduledBuildCacheSubprocess
	runScheduledBuildCacheSubprocess = func(context.Context) error {
		builds++
		_, err := buildCache(dbPath, cfg.AnalyticsDir(), true)
		return err
	}
	t.Cleanup(func() { runScheduledBuildCacheSubprocess = oldRunBuild })
	oldRefresher := daemonCacheRefresher
	daemonCacheRefresher = nil
	t.Cleanup(func() { daemonCacheRefresher = oldRefresher })

	if failAfterClear {
		// The import clears the date, then fails when it recomputes conversation
		// stats at the end of the run.
		_, err = st.DB().Exec(`CREATE TRIGGER fail_stats BEFORE UPDATE OF message_count ON conversations
 BEGIN SELECT RAISE(ABORT, 'synthetic stats failure'); END`)
		require.NoError(err)
	}

	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	runErr := runScheduledIMessage(ctx, st, config.IMessageConfig{DBPath: chatPath})
	if failAfterClear {
		require.ErrorContains(runErr, "synthetic stats failure")
		assert.Zero(builds, "a failed import does not rebuild the cache")
		assert.False(cacheStateExists(cfg), "the committed cache marker must be dropped after the date clear")
		return
	}
	require.NoError(runErr)

	assert.Equal(1, builds, "a date clear must force a rebuild despite the throttle")
	assert.Equal(0, cachedMessages("1"), "the cache must drop the message whose date was cleared")
	assert.Equal(1, cachedMessages("2"), "undisturbed messages stay cached")
}

func cacheStateExists(cfg *config.Config) bool {
	_, err := os.Stat(query.CacheStatePath(cfg.AnalyticsDir()))
	return err == nil
}

// TestScheduledIMessageUnchangedReimportNeedsNoRebuild proves a scheduled
// import that rewrites already-archived, unchanged messages leaves the
// analytics cache current, so an expired rebuild interval does not trigger a
// full rebuild. A changed message does require one.
func TestScheduledIMessageUnchangedReimportNeedsNoRebuild(t *testing.T) {
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
 INSERT INTO message VALUES ('synthetic-1', 'one', NULL, 725760000000000000, 0, 'SMS', 0, NULL);
 INSERT INTO message VALUES ('synthetic-2', 'two', NULL, 725846400000000000, 0, 'SMS', 0, NULL);
 INSERT INTO chat_message_join VALUES (1,1);
 INSERT INTO chat_message_join VALUES (1,2);
 `)
	require.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = tmp
	cfg.Analytics.AutoBuildCache = true
	cfg.Analytics.MinRebuildInterval = 0
	dbPath := cfg.DatabaseDSN()
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	fullBuilds := 0
	oldRunBuild := runScheduledBuildCacheSubprocess
	runScheduledBuildCacheSubprocess = func(ctx context.Context) error {
		if cacheNeedsBuildContext(ctx, dbPath, cfg.AnalyticsDir()).FullRebuild {
			fullBuilds++
		}
		_, err := buildCache(dbPath, cfg.AnalyticsDir(), true)
		return err
	}
	t.Cleanup(func() { runScheduledBuildCacheSubprocess = oldRunBuild })
	oldRefresher := daemonCacheRefresher
	daemonCacheRefresher = nil
	t.Cleanup(func() { daemonCacheRefresher = oldRefresher })

	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	imessageCfg := config.IMessageConfig{DBPath: chatPath}
	require.NoError(runScheduledIMessage(ctx, st, imessageCfg))
	require.Equal(1, fullBuilds, "the first import adds messages and builds the cache")

	require.NoError(runScheduledIMessage(ctx, st, imessageCfg))
	assert.Equal(1, fullBuilds, "an unchanged reimport must not require a full cache rebuild")

	_, err = chat.Exec(`UPDATE message SET text = 'one, edited' WHERE ROWID = 1`)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, imessageCfg))
	assert.Equal(2, fullBuilds, "a changed message requires a full rebuild")
}
