package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
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
 INSERT INTO message VALUES ('synthetic-1', 'one', NULL, 725760000000000000, 0, 'SMS', 0, 1);
 INSERT INTO message VALUES ('synthetic-2', 'two', NULL, 725846400000000000, 1, 'SMS', 0, 1);
 INSERT INTO chat_handle_join VALUES (1,1);
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

	buildCalls := 0
	oldRunBuild := runScheduledBuildCacheSubprocess
	runScheduledBuildCacheSubprocess = func(ctx context.Context) error {
		buildCalls++
		_, err := buildCache(dbPath, cfg.AnalyticsDir(), true)
		return err
	}
	t.Cleanup(func() { runScheduledBuildCacheSubprocess = oldRunBuild })
	oldRefresher := daemonCacheRefresher
	daemonCacheRefresher = nil
	t.Cleanup(func() { daemonCacheRefresher = oldRefresher })

	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	imessageCfg := config.IMessageConfig{DBPath: chatPath, Me: "owner@example.test"}
	require.NoError(runScheduledIMessage(ctx, st, imessageCfg))
	require.Equal(1, buildCalls, "the first import adds messages and builds the cache")
	src, err := resolveImessageSource(st)
	require.NoError(err)
	assert.True(src.LastSyncAt.Valid, "a successful scheduled import advances source freshness")

	require.NoError(runScheduledIMessage(ctx, st, imessageCfg))
	assert.Equal(1, buildCalls, "an unchanged reimport must not invoke the cache builder")
	assert.False(cacheNeedsBuildContext(ctx, dbPath, cfg.AnalyticsDir()).FullRebuild, "unchanged sender rows must leave the published cache current")

	_, err = chat.Exec(`UPDATE message SET text = 'one, edited' WHERE ROWID = 1`)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, imessageCfg))
	assert.Equal(2, buildCalls, "a changed message requires a full rebuild")
}

// TestScheduledWhatsAppFreshnessAndEditedCache exercises the daemon import
// against an existing cache, including an edit in the same run as an addition.
func TestScheduledWhatsAppFreshnessAndEditedCache(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	tmp := t.TempDir()
	path := filepath.Join(tmp, "ChatStorage.sqlite")
	chat, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = chat.Close() }()
	_, err = chat.Exec(`CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT, ZPARTNERNAME TEXT, ZSESSIONTYPE INTEGER, ZLASTMESSAGEDATE TIMESTAMP);
 CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID TEXT, ZCONTACTNAME TEXT, ZFIRSTNAME TEXT, ZISADMIN INTEGER);
 CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZGROUPMEMBER INTEGER, ZSTANZAID TEXT, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZTEXT TEXT, ZMESSAGETYPE INTEGER, ZFROMJID TEXT);
 INSERT INTO ZWACHATSESSION VALUES (1, '15555550101@s.whatsapp.net', 'Test Peer', 0, 700000001);
 INSERT INTO ZWAMESSAGE VALUES (1, 1, NULL, 'original', 0, 700000001, 'original text', 0, '15555550101@s.whatsapp.net');
 ALTER TABLE ZWAMESSAGE ADD COLUMN Z_OPT INTEGER DEFAULT 1;
 ALTER TABLE ZWAGROUPMEMBER ADD COLUMN Z_OPT INTEGER DEFAULT 1;
 CREATE TABLE Z_METADATA (Z_VERSION INTEGER PRIMARY KEY, Z_UUID VARCHAR(255), Z_PLIST BLOB);
 INSERT INTO Z_METADATA VALUES (1, 'synthetic-store', NULL);`)
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = tmp
	cfg.Analytics.AutoBuildCache = true
	cfg.Analytics.MinRebuildInterval = 0
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { _ = st.Close() }()
	require.NoError(st.InitSchema())
	buildCalls := 0
	oldBuild, oldRefresher := runScheduledBuildCacheSubprocess, daemonCacheRefresher
	runScheduledBuildCacheSubprocess = func(ctx context.Context) error {
		buildCalls++
		staleness := cacheNeedsBuildContext(ctx, cfg.DatabaseDSN(), cfg.AnalyticsDir())
		_, err := buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), staleness.FullRebuild)
		return err
	}
	daemonCacheRefresher = nil
	t.Cleanup(func() { runScheduledBuildCacheSubprocess, daemonCacheRefresher = oldBuild, oldRefresher })
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	source := config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path}
	require.NoError(runScheduledWhatsAppApple(ctx, st, source))
	require.Equal(1, buildCalls)
	sources, err := st.ListSources("whatsapp")
	require.NoError(err)
	require.Len(sources, 1)
	assert.True(sources[0].LastSyncAt.Valid)
	cursor := sources[0].SyncCursor
	require.True(cursor.Valid, "the importer persists its Apple chat markers")
	_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET last_sync_at = ? WHERE id = ?`), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), sources[0].ID)
	require.NoError(err)
	require.NoError(runScheduledWhatsAppApple(ctx, st, source))
	sources, err = st.ListSources("whatsapp")
	require.NoError(err)
	assert.True(sources[0].LastSyncAt.Time.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
	var before, after struct {
		Chats map[string]string `json:"chats"`
	}
	require.NoError(json.Unmarshal([]byte(cursor.String), &before))
	require.NoError(json.Unmarshal([]byte(sources[0].SyncCursor.String), &after))
	assert.NotEmpty(before.Chats)
	assert.Equal(before.Chats, after.Chats, "freshness must preserve the chat markers")
	assert.Equal(1, buildCalls, "an unchanged run leaves the cache current")
	_, err = chat.Exec(`UPDATE ZWAMESSAGE SET ZTEXT = 'edited text with more bytes', Z_OPT = Z_OPT + 1 WHERE Z_PK = 1;
 INSERT INTO ZWAMESSAGE VALUES (2, 1, NULL, 'addition', 0, 700000002, 'new text', 0, '15555550101@s.whatsapp.net', 1);
 UPDATE ZWACHATSESSION SET ZLASTMESSAGEDATE = 700000002 WHERE Z_PK = 1;`)
	require.NoError(err)
	require.NoError(runScheduledWhatsAppApple(ctx, st, source))
	assert.Equal(2, buildCalls, "an edit plus addition requires a full rebuild")
	analytics, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = analytics.Close() }()
	var snippet string
	var size int64
	require.NoError(analytics.QueryRow(`SELECT snippet, size_estimate FROM read_parquet(?, hive_partitioning=true) WHERE source_message_id = 'original'`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&snippet, &size))
	assert.Equal("edited text with more bytes", snippet)
	assert.Equal(int64(len("edited text with more bytes")), size)
}

func TestScheduledWhatsAppPersistsDuplicateWarnings(t *testing.T) {
	for _, onlyDuplicates := range []bool{false, true} {
		t.Run(strconv.FormatBool(onlyDuplicates), func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			tmp := t.TempDir()
			path := filepath.Join(tmp, "ChatStorage.sqlite")
			chat, err := sql.Open("sqlite3", path)
			require.NoError(err)
			defer func() { _ = chat.Close() }()
			_, err = chat.Exec(`CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT, ZPARTNERNAME TEXT, ZSESSIONTYPE INTEGER, ZLASTMESSAGEDATE TIMESTAMP);
 CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID TEXT, ZCONTACTNAME TEXT, ZFIRSTNAME TEXT, ZISADMIN INTEGER);
 CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZGROUPMEMBER INTEGER, ZSTANZAID TEXT, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZTEXT TEXT, ZMESSAGETYPE INTEGER, ZFROMJID TEXT);
 INSERT INTO ZWACHATSESSION VALUES (1, '15555550101@s.whatsapp.net', 'Test Peer', 0, 700000001);
 INSERT INTO ZWAMESSAGE VALUES (1, 1, NULL, 'original', 0, 700000001, 'original text', 0, '15555550101@s.whatsapp.net');
 ALTER TABLE ZWAMESSAGE ADD COLUMN Z_OPT INTEGER DEFAULT 1;
 ALTER TABLE ZWAGROUPMEMBER ADD COLUMN Z_OPT INTEGER DEFAULT 1;
 CREATE TABLE Z_METADATA (Z_VERSION INTEGER PRIMARY KEY, Z_UUID VARCHAR(255), Z_PLIST BLOB);
 INSERT INTO Z_METADATA VALUES (1, 'synthetic-store', NULL);`)
			require.NoError(err)
			_, err = chat.Exec(`INSERT INTO ZWAMESSAGE VALUES
 (2, 1, NULL, 'duplicate', 0, 700000002, 'duplicate one', 0, '15555550101@s.whatsapp.net', 1),
 (3, 1, NULL, 'duplicate', 0, 700000003, 'duplicate two', 0, '15555550101@s.whatsapp.net', 1)`)
			require.NoError(err)
			if onlyDuplicates {
				_, err = chat.Exec(`DELETE FROM ZWAMESSAGE WHERE Z_PK = 1`)
				require.NoError(err)
			}
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = tmp
			cfg.Analytics.AutoBuildCache = false
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			defer func() { _ = st.Close() }()
			require.NoError(st.InitSchema())
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			var logs bytes.Buffer
			invocationFromContext(ctx).logger = slog.New(slog.NewTextHandler(&logs, nil))
			source := config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path}
			require.NoError(runScheduledWhatsAppApple(ctx, st, source))
			var status string
			var errorsCount, messages int
			require.NoError(st.DB().QueryRow(`SELECT status, errors_count FROM sync_runs`).Scan(&status, &errorsCount))
			assert.Equal("completed", status)
			assert.Equal(1, errorsCount, "duplicate warnings must survive in source status")
			assert.Contains(logs.String(), "level=WARN")
			assert.Contains(logs.String(), "skipping 2 Apple text messages whose stanza IDs are duplicated")
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			if onlyDuplicates {
				assert.Zero(messages)
			} else {
				assert.Equal(1, messages)
			}
		})
	}
}
