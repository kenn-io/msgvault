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

// An archived Apple source whose database cannot be opened must show the
// failure, not the previous run's success, and must not force a full cache
// rebuild on every scheduled attempt.
func TestScheduledAppleImportFailingToOpenRecordsFailedRun(t *testing.T) {
	missing := func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.sqlite") }
	for _, tc := range []struct {
		name       string
		sourceType string
		run        func(ctx context.Context, st *store.Store, path string) error
		fixture    func(t *testing.T) string
	}{
		{
			name: "imessage", sourceType: sourceTypeAppleMessages,
			run: func(ctx context.Context, st *store.Store, path string) error {
				return runScheduledIMessage(ctx, st, config.IMessageConfig{DBPath: path})
			},
			fixture: func(t *testing.T) string {
				path, chat := testutil.NewIMessageChatDB(t)
				_, err := chat.ExecContext(t.Context(), `CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
 INSERT INTO message (date, text) VALUES (725760000, 'hello')`)
				require.NoError(t, err)
				return path
			},
		},
		{
			name: "whatsapp", sourceType: "whatsapp",
			run: func(ctx context.Context, st *store.Store, path string) error {
				return runScheduledWhatsAppApple(ctx, st, config.WhatsAppAppleSource{Phone: "+15550000000", Path: path})
			},
			fixture: func(t *testing.T) string { return createWhatsAppIdentityFixture(t, false, false) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Analytics.AutoBuildCache = false
			st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			path := tc.fixture(t)
			require.NoError(tc.run(ctx, st, path))
			_, err := buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
			require.NoError(err)

			runErr := tc.run(ctx, st, missing(t))
			require.Error(runErr)

			sources, err := st.ListSourcesContext(t.Context(), tc.sourceType)
			require.NoError(err)
			require.Len(sources, 1)
			run, err := st.GetLatestSyncContext(t.Context(), sources[0].ID, 0)
			require.NoError(err)
			assert.Equal(store.SyncStatusFailed, run.Status)
			require.True(run.ErrorMessage.Valid)
			assert.Equal(runErr.Error(), run.ErrorMessage.String)
			assert.False(cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir()).NeedsBuild,
				"an attempt that wrote nothing must not force a cache rebuild")

			require.NoError(tc.run(ctx, st, path))
			run, err = st.GetLatestSyncContext(t.Context(), sources[0].ID, 0)
			require.NoError(err)
			assert.Equal(store.SyncStatusCompleted, run.Status)
		})
	}

	t.Run("never imported", func(t *testing.T) {
		require := require.New(t)
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = t.TempDir()
		cfg.Analytics.AutoBuildCache = false
		st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
		ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
		require.Error(runScheduledIMessage(ctx, st, config.IMessageConfig{DBPath: missing(t)}))
		require.Error(runScheduledWhatsAppApple(ctx, st, config.WhatsAppAppleSource{Phone: "+15550000000", Path: missing(t)}))
		var runs int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sync_runs`).Scan(&runs))
		assert.Zero(t, runs)
	})
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
	_, err = importImessageRecorded(t.Context(), st, client, src.ID)
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
	_, err = importImessageRecorded(t.Context(), st, client, src.ID)
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
	_, err = importImessageRecorded(t.Context(), st, client, src.ID)
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
	_, err = importImessageRecorded(t.Context(), st, client, src.ID)
	require.ErrorIs(err, imessage.ErrAttachmentImport)
	require.ErrorContains(err, "synthetic archive failure")
	run, err = st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("failed", run.Status)
}
