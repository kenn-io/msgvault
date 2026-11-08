package cmd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"

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
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRunScheduledIMessage(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	path, chat := testutil.NewIMessageChatDB(t)
	cfg.IMessage.DBPath = path
	cfg.IMessage.Me = "owner@example.test"
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	src, err := resolveImessageSource(st)
	req.NoError(err)
	_, err = chat.ExecContext(t.Context(), `CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
 INSERT INTO message (date,text) VALUES (?, 'recent'), (?, 'old')`,
		time.Now().Add(-time.Hour).Unix()-978307200, time.Now().Add(-30*24*time.Hour).Unix()-978307200)
	req.NoError(err)
	req.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	first, err := st.GetLastSuccessfulSyncByType(t.Context(), src.ID, "imessage_import")
	req.NoError(err)
	check.Equal("completed", first.Status)
	check.Equal(int64(2), first.MessagesAdded)
	src, err = st.GetSourceByID(src.ID)
	req.NoError(err)
	check.True(src.LastSyncAt.Valid)
	_, err = chat.ExecContext(t.Context(), `INSERT INTO message (date,text) VALUES (?, 'late'), (?, 'older')`,
		time.Now().Add(-7*24*time.Hour).Unix()-978307200, time.Now().Add(-30*24*time.Hour).Unix()-978307200)
	req.NoError(err)
	req.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	var ids string
	req.NoError(st.DB().QueryRowContext(t.Context(), `SELECT group_concat(source_message_id, ',') FROM (SELECT source_message_id FROM messages ORDER BY source_message_id)`).Scan(&ids))
	check.Equal("1,2,3,4", ids)
	t.Run("retitling leaves the disabled cache stale", func(t *testing.T) {
		req := require.New(t)
		check := assert.New(t)
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = t.TempDir()
		cfg.Analytics.AutoBuildCache = false
		path, chat := testutil.NewIMessageChatDB(t)
		cfg.IMessage.DBPath = path
		_, err := chat.ExecContext(t.Context(), `CREATE TABLE attachment (filename TEXT);
	 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
	 INSERT INTO chat VALUES ('any;-;synthetic', NULL, 'peer@example.test');
	 INSERT INTO message (date,text) VALUES (725760000,'hello');
	 INSERT INTO chat_message_join VALUES (1,1)`)
		req.NoError(err)
		st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
		ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
		req.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
		_, err = buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
		req.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `UPDATE participants SET display_name = 'Renamed peer' WHERE email_address = 'peer@example.test'`)
		req.NoError(err)
		req.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
		var title string
		req.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title FROM conversations`).Scan(&title))
		check.Equal("Renamed peer", title)
		stale := cacheNeedsBuildForScheduledRefresh(ctx, cfg.DatabaseDSN(), cfg.AnalyticsDir())
		check.True(stale.NeedsBuild)
		check.True(stale.HasConversationTypeDrift)
		check.False(stale.FullRebuild)
	})

	t.Run("scheduled import with automatic cache disabled", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = t.TempDir()
		cfg.Analytics.AutoBuildCache = false
		path, chat := testutil.NewIMessageChatDB(t)
		cfg.IMessage.DBPath = path
		cfg.IMessage.Me = "owner@example.test"
		_, err := chat.ExecContext(t.Context(), `CREATE TABLE attachment (filename TEXT);
	 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
	 INSERT INTO chat VALUES ('any;-;synthetic', NULL, 'peer@example.test');
	 INSERT INTO message (date, text) VALUES (?, 'Synthetic message');
	 INSERT INTO chat_message_join VALUES (1,1);`, time.Now().UnixNano()-978307200000000000)
		require.NoError(err)
		st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
		ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
		require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
		src, err := resolveImessageSource(st)
		require.NoError(err)
		require.NoError(st.AddAccountIdentity(src.ID, cfg.IMessage.Me, "manual"))
		_, err = buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
		require.NoError(err)
		assert.False(cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir()).NeedsBuild)
		_, err = chat.ExecContext(t.Context(), `UPDATE message SET text = 'Edited message'`)
		require.NoError(err)
		require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
		edited := cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir())
		assert.True(edited.NeedsBuild)
		assert.True(edited.FullRebuild)
		_, err = buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
		require.NoError(err)
		_, err = chat.ExecContext(t.Context(), `UPDATE message SET date = ?`, int64(math.MaxInt64))
		require.NoError(err)
		require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
		run, err := st.GetLastSuccessfulSyncByType(t.Context(), src.ID, "imessage_import")
		require.NoError(err)
		assert.Equal(int64(1), run.MessagesUpdated)
		stale := cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir())
		assert.True(stale.NeedsBuild)
		assert.True(stale.FullRebuild)
	})
}

func TestRunScheduledWhatsAppApple(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	source := config.WhatsAppAppleSource{Phone: "+15550000000", Path: createWhatsAppIdentityFixture(t, false, false)}
	req.NoError(runScheduledWhatsAppApple(ctx, st, source))
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), "whatsapp", source.Phone)
	req.NoError(err)
	run, err := st.GetLatestSync(src.ID)
	req.NoError(err)
	check.Equal("completed", run.Status)
	check.Equal(int64(1), run.MessagesAdded)
	identities, err := st.ListAccountIdentities(src.ID)
	req.NoError(err)
	req.Len(identities, 1)
	check.Equal(source.Phone, identities[0].Address)
	db, err := sql.Open("sqlite3", source.Path)
	req.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(t.Context(), `UPDATE ZWAMESSAGE SET ZTEXT = 'Edited message'`)
	req.NoError(err)
	req.NoError(runScheduledWhatsAppApple(ctx, st, source))
	second, err := st.GetLatestSync(src.ID)
	req.NoError(err)
	check.Equal(int64(0), second.MessagesAdded)
	check.Equal(int64(1), second.MessagesUpdated)
	check.Greater(second.ID, run.ID)
}

func TestImportImessageRecordedPreservesCountersAfterCheckpointFailure(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	path, chat := testutil.NewIMessageChatDB(t)
	_, err := chat.ExecContext(t.Context(), `INSERT INTO message (date,text) VALUES (725760000,'one'), (725760001,'two')`)
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs
		WHEN NEW.status = 'running'
		BEGIN SELECT RAISE(ABORT, 'synthetic checkpoint failure'); END`)
	require.NoError(err)
	client, err := imessage.NewClient(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)
	require.ErrorContains(err, "synthetic checkpoint failure")
	run, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("failed", run.Status)
	assert.Equal(int64(2), run.MessagesProcessed)
	assert.Equal(int64(2), run.MessagesAdded)
}

func TestImportImessageRecordedPreservesPartialCounters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, chat := testutil.NewIMessageChatDB(t)
	_, err := chat.ExecContext(t.Context(), `INSERT INTO message (date,text) VALUES (725760000,'one'), (725760001,'two'), (725760002,'three')`)
	require.NoError(err)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "synthetic.txt"), []byte("synthetic attachment"), 0600))
	_, err = chat.ExecContext(t.Context(), `CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
 INSERT INTO attachment VALUES (?);
 INSERT INTO message_attachment_join VALUES (3,1);
 UPDATE message SET cache_has_attachments = 1 WHERE ROWID = 3`, filepath.Join(root, "synthetic.txt"))
	require.NoError(err)
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "msgvault.db"))
	src, err := resolveImessageSource(st)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_message BEFORE INSERT ON messages WHEN NEW.source_message_id = '2' BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END; CREATE TRIGGER fail_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'synthetic fatal failure'); END`)
	require.NoError(err)
	client, err := imessage.NewClient(t.Context(), path, imessage.WithAttachments(t.TempDir(), root, config.IMessageConfig{}.MediaPolicy()))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)
	require.ErrorContains(err, "synthetic fatal failure")
	run, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("failed", run.Status)
	assert.Equal(int64(3), run.MessagesProcessed)
	assert.Equal(int64(2), run.MessagesAdded)
	assert.Equal(int64(2), run.ErrorsCount)
}

func TestRunScheduledIMessageFailureRequestsRefresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = true
	cfg.IMessage.DBPath, _ = testutil.NewIMessageChatDB(t)
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	requested := make(chan string, 1)
	refresher := newBackgroundCacheRefresher(ctx, func(_ context.Context, id string) error {
		requested <- id
		return nil
	}, nil)
	previous := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		require.NoError(refresher.Shutdown(context.WithoutCancel(t.Context())))
		daemonCacheRefresher = previous
	})
	// Opening the iMessage database honors the cancelled context, so this run
	// stops before it starts a sync run. It must still request the refresh.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(runScheduledIMessage(canceled, st, cfg.IMessage), context.Canceled)
	select {
	case id := <-requested:
		assert.Equal("apple-messages", id)
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("failed import did not request a refresh")
	}
}

func TestImportImessageRecordedRecordsWarningsAndFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, chat := testutil.NewIMessageAttachmentChatDB(t)
	home := t.TempDir()
	setTestHome(t, home)
	root := filepath.Join(home, "Library", "Messages")
	require.NoError(os.MkdirAll(filepath.Join(root, "Attachments"), 0700))
	require.NoError(os.WriteFile(filepath.Join(root, "Attachments", "voice.caf"), []byte("caffSYNTHETIC"), 0600))
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	cfg.IMessage.DBPath = path
	dest := cfg.AttachmentsDir()
	require.NoError(os.WriteFile(dest, []byte("synthetic blocker"), 0600))
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	src, err := resolveImessageSource(st)
	require.NoError(err)
	client, err := imessage.NewClient(t.Context(), path, imessage.WithAttachments(dest, root, config.IMessageConfig{}.MediaPolicy()))
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)
	require.ErrorIs(err, imessage.ErrAttachmentImport)
	run, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	assert.Equal(int64(2), run.ErrorsCount)
	src, err = st.GetSourceByID(src.ID)
	require.NoError(err)
	assert.True(src.LastSyncAt.Valid)
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	run, err = st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	assert.Equal(int64(2), run.ErrorsCount)
	// Let attachment capture reach the archive write below.
	require.NoError(os.Remove(dest))
	// An archive write failure also carries ErrAttachmentImport, but fails the run.
	_, err = chat.ExecContext(t.Context(), `INSERT INTO message (date,text,cache_has_attachments) VALUES (700000002,'Third',1);
 INSERT INTO chat_message_join VALUES (1,3); INSERT INTO message_attachment_join VALUES (3,1)`)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'synthetic archive failure'); END`)
	require.NoError(err)
	_, err = importImessageRecorded(t.Context(), st, client, src.ID, path)
	require.ErrorIs(err, imessage.ErrAttachmentImport)
	require.ErrorContains(err, "synthetic archive failure")
	run, err = st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("failed", run.Status)
}

// A date clear marks the cache stale while scheduled refresh honors its
// minimum rebuild interval, including when the import subsequently fails.
func TestScheduledIMessageReimportDefersCacheAfterDateClear(t *testing.T) {
	scheduledIMessageDateClear(t, false)
}

func TestScheduledIMessageFailedImportDefersCacheAfterDateClear(t *testing.T) {
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
 CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
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
	c, err := imessage.NewClient(t.Context(), chatPath)
	require.NoError(err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.Import(t.Context(), st, src.ID)
	require.NoError(err)
	_, err = buildCache(t.Context(), dbPath, cfg.AnalyticsDir(), true)
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
		_, err := buildCache(t.Context(), dbPath, cfg.AnalyticsDir(), true)
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
	} else {
		require.NoError(runErr)
	}
	assert.Zero(builds, "scheduled refresh honors the minimum rebuild interval")
	assert.True(cacheStateExists(cfg), "the published cache stays available until its refresh")
	staleness := cacheNeedsBuildContext(ctx, dbPath, cfg.AnalyticsDir())
	require.True(staleness.NeedsBuild, "the date clear must leave the cache stale")
	require.True(staleness.FullRebuild, "the next refresh must replace the cleared date")

	cfg.Analytics.MinRebuildInterval = 0
	require.NoError(rebuildCacheAfterScheduledSync(ctx, "apple-messages"))
	assert.Equal(1, builds, "the next eligible refresh rebuilds the stale cache")
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
 CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
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
		_, err := buildCache(t.Context(), dbPath, cfg.AnalyticsDir(), true)
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
		_, err := buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), staleness.FullRebuild)
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
