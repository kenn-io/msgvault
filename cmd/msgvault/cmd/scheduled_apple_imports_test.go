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
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRunScheduledIMessageRecordsRunsAndLateArrivals(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	path, chat := testutil.NewIMessageChatDB(t)
	cfg.IMessage.DBPath = path
	cfg.IMessage.Me = "owner@example.test"
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	// A manual, limited import must not anchor the first scheduled scan.
	src, err := resolveImessageSource(st)
	require.NoError(err)
	manual, err := st.StartSync(src.ID, "imessage_import")
	require.NoError(err)
	require.NoError(st.CompleteSync(manual, ""))
	_, err = chat.Exec(`CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
 INSERT INTO message (date,text) VALUES (?, 'recent'), (?, 'old')`,
		time.Now().Add(-time.Hour).Unix()-978307200, time.Now().Add(-30*24*time.Hour).Unix()-978307200)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	first, err := st.GetLastSuccessfulSyncByType(src.ID, "imessage_import")
	require.NoError(err)
	assert.Equal("completed", first.Status)
	assert.Equal(int64(2), first.MessagesAdded)
	src, err = st.GetSourceByID(src.ID)
	require.NoError(err)
	assert.True(src.LastSyncAt.Valid)
	_, err = chat.Exec(`INSERT INTO message (date,text) VALUES (?, 'late'), (?, 'older')`,
		time.Now().Add(-7*24*time.Hour).Unix()-978307200, time.Now().Add(-30*24*time.Hour).Unix()-978307200)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	second, err := st.GetLastSuccessfulSyncByType(src.ID, "imessage_import")
	require.NoError(err)
	assert.Greater(second.ID, first.ID)
	assert.Equal(int64(2), second.MessagesAdded)
	var ids string
	require.NoError(st.DB().QueryRow(`SELECT group_concat(source_message_id, ',') FROM (SELECT source_message_id FROM messages ORDER BY source_message_id)`).Scan(&ids))
	assert.Equal("1,2,3,4", ids)
}

func TestScheduledIMessageDateClearForcesFullCacheRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	path, chat := testutil.NewIMessageChatDB(t)
	cfg.IMessage.DBPath = path
	cfg.IMessage.Me = "owner@example.test"
	_, err := chat.Exec(`CREATE TABLE attachment (filename TEXT);
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
	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	assert.False(cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir()).NeedsBuild)
	_, err = chat.Exec(`UPDATE message SET text = 'Edited message'`)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	edited := cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir())
	assert.True(edited.NeedsBuild)
	assert.True(edited.FullRebuild)
	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	require.NoError(err)
	_, err = chat.Exec(`UPDATE message SET date = ?`, int64(math.MaxInt64))
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	run, err := st.GetLastSuccessfulSyncByType(src.ID, "imessage_import")
	require.NoError(err)
	assert.Equal(int64(1), run.MessagesUpdated)
	stale := cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir())
	assert.True(stale.NeedsBuild)
	assert.True(stale.FullRebuild)
}

func TestRunScheduledIMessageRequestsRefreshAfterFailure(t *testing.T) {
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
		require.NoError(refresher.Shutdown(context.Background()))
		daemonCacheRefresher = previous
	})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(runScheduledIMessage(canceled, st, cfg.IMessage), context.Canceled)
	src, err := resolveImessageSource(st)
	require.NoError(err)
	run, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("failed", run.Status)
	select {
	case id := <-requested:
		assert.Equal("apple-messages", id)
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("failed import did not request a refresh")
	}
}

func TestRunScheduledWhatsAppApple(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	source := config.WhatsAppAppleSource{Phone: "+15550000000", Path: createWhatsAppIdentityFixture(t, false, false)}
	require.NoError(runScheduledWhatsAppApple(ctx, st, source))
	src, err := st.GetSourceByTypeAndIdentifier("whatsapp", source.Phone)
	require.NoError(err)
	assert.True(src.LastSyncAt.Valid)
	run, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	assert.Equal(int64(1), run.MessagesAdded)
	require.True(run.CompletedAt.Valid)
	assert.Equal(run.CompletedAt.Time, src.LastSyncAt.Time)
	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal(source.Phone, identities[0].Address)
	db, err := sql.Open("sqlite3", source.Path)
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`UPDATE ZWAMESSAGE SET ZTEXT = 'Edited message'`)
	require.NoError(err)
	require.NoError(runScheduledWhatsAppApple(ctx, st, source))
	second, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal(int64(0), second.MessagesAdded)
	assert.Equal(int64(1), second.MessagesUpdated)
	assert.Greater(second.ID, run.ID)
}

func TestImportImessageRecordedPreservesPartialCounters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, chat := testutil.NewIMessageChatDB(t)
	_, err := chat.Exec(`INSERT INTO message (date,text) VALUES (725760000,'one'), (725760001,'two')`)
	require.NoError(err)
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(t.TempDir(), "msgvault.db"))
	src, err := resolveImessageSource(st)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_message BEFORE INSERT ON messages WHEN NEW.source_message_id = '2' BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`)
	require.NoError(err)
	client, err := imessage.NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = importImessageRecorded(t.Context(), st, client, src.ID)
	require.NoError(err)
	run, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal(int64(2), run.MessagesProcessed)
	assert.Equal(int64(1), run.MessagesAdded)
	assert.Equal(int64(1), run.ErrorsCount)
}

func TestImportImessageRecordedCompletesWithAttachmentErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, chat := testutil.NewIMessageAttachmentChatDB(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
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
	client, err := imessage.NewClient(path, imessage.WithAttachments(dest, root, config.IMessageConfig{}.MediaPolicy()))
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
	// Even unchanged messages retry attachment capture on the next pass.
	require.NoError(os.Remove(dest))
	_, err = importImessageRecorded(t.Context(), st, client, src.ID)
	require.NoError(err)
	run, err = st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Zero(run.MessagesAdded)
	assert.Zero(run.MessagesUpdated)
	assert.Zero(run.ErrorsCount)
	var attachments int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE storage_path IS NOT NULL AND storage_path != ''`).Scan(&attachments))
	assert.Equal(2, attachments)
	// An archive write failure also carries ErrAttachmentImport, but fails the run.
	_, err = chat.Exec(`INSERT INTO message (date,text,cache_has_attachments) VALUES (700000002,'Third',1);
 INSERT INTO chat_message_join VALUES (1,3); INSERT INTO message_attachment_join VALUES (3,1)`)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'synthetic archive failure'); END`)
	require.NoError(err)
	_, err = importImessageRecorded(t.Context(), st, client, src.ID)
	require.ErrorIs(err, imessage.ErrAttachmentImport)
	require.ErrorContains(err, "synthetic archive failure")
	run, err = st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal("failed", run.Status)
}

func TestScheduledAppleOpenFailureRecordsRun(t *testing.T) {
	for _, provider := range []string{"apple_messages", "whatsapp"} {
		t.Run(provider, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Analytics.AutoBuildCache = false
			st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			var path, identifier string
			var run func() error
			if provider == "apple_messages" {
				var chat *sql.DB
				path, chat = testutil.NewIMessageChatDB(t)
				_, err := chat.Exec(`CREATE TABLE attachment (filename TEXT); CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER)`)
				require.NoError(err)
				cfg.IMessage.DBPath = path
				identifier = "local"
				run = func() error { return runScheduledIMessage(ctx, st, cfg.IMessage) }
			} else {
				path = createWhatsAppIdentityFixture(t, false, false)
				identifier = "+15550000000"
				run = func() error {
					return runScheduledWhatsAppApple(ctx, st, config.WhatsAppAppleSource{Phone: identifier, Path: path})
				}
			}
			require.NoError(run())
			src, err := st.GetSourceByTypeAndIdentifier(provider, identifier)
			require.NoError(err)
			first, err := st.GetLatestSync(src.ID)
			require.NoError(err)
			require.Equal("completed", first.Status)
			require.NoError(os.Remove(path))
			require.Error(run())
			latest, err := st.GetLatestSync(src.ID)
			require.NoError(err)
			assert.Greater(latest.ID, first.ID)
			assert.Equal("failed", latest.Status)
		})
	}
}

func TestScheduledAppleImportsRecordFailures(t *testing.T) {
	for _, source := range []string{"whatsapp", "apple_messages"} {
		t.Run(source, func(t *testing.T) {
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Analytics.AutoBuildCache = false
			cfg.IMessage.DBPath = filepath.Join(t.TempDir(), "missing.db")
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			var sources []any
			invocationFromContext(ctx).syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
				assert.Equal(t, telemetry.EventSyncFailed, event)
				sources = append(sources, props["sync_source"])
				return nil
			}}
			st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
			if source == "whatsapp" {
				require.Error(t, runScheduledWhatsAppApple(ctx, st, config.WhatsAppAppleSource{Phone: "+15550000000", Path: cfg.IMessage.DBPath}))
			} else {
				require.Error(t, runScheduledIMessage(ctx, st, cfg.IMessage))
			}
			assert.Equal(t, []any{source}, sources)
		})
	}
}

func TestScheduledIMessageRetitleHonorsCachePolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	path, chat := testutil.NewIMessageChatDB(t)
	cfg.IMessage.DBPath = path
	_, err := chat.Exec(`CREATE TABLE attachment (filename TEXT);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
 INSERT INTO chat VALUES ('any;-;synthetic', NULL, 'peer@example.test');
 INSERT INTO message (date,text) VALUES (725760000,'hello');
 INSERT INTO chat_message_join VALUES (1,1)`)
	require.NoError(err)
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	_, err = os.Stat(cfg.AnalyticsDir())
	require.ErrorIs(err, os.ErrNotExist)
	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE participants SET display_name = 'Renamed peer' WHERE email_address = 'peer@example.test'`)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	stale := cacheNeedsBuildForScheduledRefresh(ctx, cfg.DatabaseDSN(), cfg.AnalyticsDir())
	assert.True(stale.NeedsBuild)
	assert.True(stale.HasConversationTypeDrift)
	assert.False(stale.FullRebuild)

	requested := make(chan string, 1)
	refresher := newBackgroundCacheRefresher(ctx, func(_ context.Context, id string) error {
		requested <- id
		return nil
	}, nil)
	previous := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		require.NoError(refresher.Shutdown(context.Background()))
		daemonCacheRefresher = previous
	})
	cfg.Analytics.AutoBuildCache = true
	_, err = st.DB().Exec(`UPDATE participants SET display_name = 'Renamed again' WHERE email_address = 'peer@example.test'`)
	require.NoError(err)
	require.NoError(runScheduledIMessage(ctx, st, cfg.IMessage))
	select {
	case id := <-requested:
		assert.Equal("apple-messages", id)
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("retitle did not queue a refresh")
	}
}
