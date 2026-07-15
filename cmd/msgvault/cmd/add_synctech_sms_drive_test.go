package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAddSynctechSMSDriveWritesConfigWithoutSecrets(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cmd := newTestRootCmd()
	cmd.AddCommand(newAddSynctechSMSDriveCmd())
	cmd.SetArgs([]string{
		"add-synctech-sms-drive", "pixel",
		"--owner-phone", "+15550000001",
		"--folder-id", "drive-folder-id",
		"--google-account", "user@example.com",
		"--schedule", "30 4 * * *",
		"--oauth-app", "personal",
		"--skip-auth-for-test",
	})
	require.NoError(cmd.Execute(), "Execute")
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(err, "read config")
	text := string(data)
	for _, want := range []string{`[[synctech_sms.sources]]`, `name = "pixel"`, `backend = "drive"`, `folder_id = "drive-folder-id"`, `google_account = "user@example.com"`, `owner_phone = "+15550000001"`} {
		require.Contains(text, want, "config missing %q", want)
	}
	lower := strings.ToLower(text)
	refreshTokenKey := "refresh" + "_token"
	clientSecretKey := "client" + "_secret\""
	assert.NotContains(lower, refreshTokenKey, "config contains secret material:\n%s", text)
	assert.NotContains(lower, clientSecretKey, "config contains secret material:\n%s", text)
}

func TestSynctechSMSDriveRunUsesSingleOuterSyncRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	f := storetest.New(t)
	src := synctechDriveTestSource()
	file := synctechsms.DriveFile{
		ID:           "backup-1",
		Name:         "sms.xml",
		Checksum:     "sum-1",
		Size:         128,
		ModifiedTime: time.Now().Add(-20 * time.Minute),
	}
	client := fakeSynctechDriveClient{
		files: []synctechsms.DriveFile{file},
		downloads: map[string]string{
			"backup-1": `<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="hello from drive" read="1" status="-1" contact_name="Alice" />
</smses>`,
		},
	}

	summary, err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client)
	require.NoError(err, "runSynctechSMSDriveSourceWithClient")
	require.Len(summary.MessageIDs, 1, "summary message IDs")

	source := getSynctechSource(t, f.Store, src.OwnerPhone)
	assert.Equal(1, countSyncRuns(t, f.Store, source.ID), "sync run count")
	run := getOnlySyncRun(t, f.Store, source.ID)
	assert.Equal(store.SyncStatusCompleted, run.Status, "sync status")
	assert.Equal(int64(1), run.MessagesProcessed, "messages processed")
	assert.Equal(int64(1), run.MessagesAdded, "messages added")
	assert.True(getSynctechSource(t, f.Store, src.OwnerPhone).LastSyncAt.Valid, "last_sync_at should be touched")

	item := getDriveSourceImportItem(t, f.Store, source.ID, "backup-1")
	assert.Equal("imported", item.Status, "source import status")
	assert.Equal(1, item.RecordsImported, "records imported")
	assert.False(item.ErrorMessage.Valid, "source import error")
	assertSourceMessageCount(t, f.Store, source.ID, 1)
	assertSourceConversationMessageCount(t, f.Store, source.ID, 1)
}

func TestSynctechSMSDriveRunSetsUpIdentityAndPostSourceMigration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() {
		cfg = savedCfg
	})
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cfg.Identity.Addresses = []string{"legacy@example.com"}
	st := testutil.NewTestStore(t)
	emailSource, err := st.GetOrCreateSource("gmail", "mailbox@example.com")
	require.NoError(err, "GetOrCreateSource")

	src := synctechDriveTestSource()
	client := fakeSynctechDriveClient{}

	_, err = runSynctechSMSDriveSourceWithClient(context.Background(), st, src, synctechImportOptions(src), client)
	require.NoError(err, "runSynctechSMSDriveSourceWithClient")

	synctechSource := getSynctechSource(t, st, src.OwnerPhone)
	synctechIDs, err := st.ListAccountIdentities(synctechSource.ID)
	require.NoError(err, "ListAccountIdentities synctech")
	require.Len(synctechIDs, 1, "Synctech should keep only its owner-phone identity")
	assert.Equal(src.OwnerPhone, synctechIDs[0].Address, "Synctech identity address")
	assert.Equal("account-identifier", synctechIDs[0].SourceSignal, "Synctech identity signal")

	emailIDs, err := st.ListAccountIdentities(emailSource.ID)
	require.NoError(err, "ListAccountIdentities gmail")
	require.Len(emailIDs, 1, "post-source migration should run for eligible email sources")
	assert.Equal("legacy@example.com", emailIDs[0].Address, "migrated identity address")
	assert.Equal("config_migration", emailIDs[0].SourceSignal, "migrated identity signal")

	applied, err := st.IsMigrationApplied("legacy_identity_to_per_account")
	require.NoError(err, "IsMigrationApplied")
	assert.True(applied, "post-source migration sentinel should be set")
}

func TestSynctechSMSDriveRunRecordsZeroSelectedPoll(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	f := storetest.New(t)
	src := synctechDriveTestSource()
	src.StableAfter = "1h"
	client := fakeSynctechDriveClient{
		files: []synctechsms.DriveFile{{
			ID:           "backup-1",
			Name:         "sms.xml",
			Checksum:     "sum-1",
			Size:         128,
			ModifiedTime: time.Now().Add(-5 * time.Minute),
		}},
	}

	_, err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client)
	require.NoError(err, "runSynctechSMSDriveSourceWithClient")

	source := getSynctechSource(t, f.Store, src.OwnerPhone)
	assert.Equal(1, countSyncRuns(t, f.Store, source.ID), "sync run count")
	run := getOnlySyncRun(t, f.Store, source.ID)
	assert.Equal(store.SyncStatusCompleted, run.Status, "sync status")
	assert.Equal(int64(0), run.MessagesProcessed, "messages processed")
	assert.Equal(int64(0), run.MessagesAdded, "messages added")
	assert.True(getSynctechSource(t, f.Store, src.OwnerPhone).LastSyncAt.Valid, "last_sync_at should be touched")
	assertSourceMessageCount(t, f.Store, source.ID, 0)
}

func TestSynctechSMSDriveRunMarksOuterSyncFailedOnDownloadError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	f := storetest.New(t)
	src := synctechDriveTestSource()
	downloadErr := errors.New("download unavailable")
	client := fakeSynctechDriveClient{
		files: []synctechsms.DriveFile{{
			ID:           "backup-1",
			Name:         "sms.xml",
			Checksum:     "sum-1",
			Size:         128,
			ModifiedTime: time.Now().Add(-20 * time.Minute),
		}},
		downloadErr: downloadErr,
	}

	_, err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client)
	require.ErrorIs(err, downloadErr, "runSynctechSMSDriveSourceWithClient")

	source := getSynctechSource(t, f.Store, src.OwnerPhone)
	assert.Equal(1, countSyncRuns(t, f.Store, source.ID), "sync run count")
	run := getOnlySyncRun(t, f.Store, source.ID)
	assert.Equal(store.SyncStatusFailed, run.Status, "sync status")
	require.True(run.ErrorMessage.Valid, "sync error_message")
	assert.Contains(run.ErrorMessage.String, downloadErr.Error(), "sync error_message")

	item := getDriveSourceImportItem(t, f.Store, source.ID, "backup-1")
	assert.Equal("failed", item.Status, "source import status")
	require.True(item.ErrorMessage.Valid, "source import error")
	assert.Contains(item.ErrorMessage.String, downloadErr.Error(), "source import error")
}

func TestSynctechSMSDrivePartialFailureEnqueuesImportedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	f := storetest.New(t)
	src := synctechDriveTestSource()
	client := fakeSynctechDriveClient{
		files: []synctechsms.DriveFile{
			{
				ID:           "backup-1",
				Name:         "sms-1.xml",
				Checksum:     "sum-1",
				Size:         128,
				ModifiedTime: time.Now().Add(-30 * time.Minute),
			},
			{
				ID:           "backup-2",
				Name:         "sms-2.xml",
				Checksum:     "sum-2",
				Size:         128,
				ModifiedTime: time.Now().Add(-30 * time.Minute),
			},
		},
		downloads: map[string]string{
			"backup-1": `<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="hello before failure" read="1" status="-1" contact_name="Alice" />
</smses>`,
			"backup-2": `<smses count="1">
  <sms address="+15557654321" date="1717214460000" type="1" body="durable before malformed tail" read="1" status="-1" contact_name="Bob" />
  <sms`,
		},
	}
	err := runConfiguredSynctechSMSSourceWithStoreDriveClient(
		context.Background(), f.Store, src, client)
	require.Error(err, "runConfiguredSynctechSMSSourceWithStoreDriveClient")
	assert.Contains(err.Error(), "import backup file", "partial parse error")

	source := getSynctechSource(t, f.Store, src.OwnerPhone)
	assertSourceMessageCount(t, f.Store, source.ID, 2)
	assert.Equal(1, countSyncRuns(t, f.Store, source.ID), "sync run count")
	run := getOnlySyncRun(t, f.Store, source.ID)
	assert.Equal(store.SyncStatusFailed, run.Status, "sync status")

	imported := getDriveSourceImportItem(t, f.Store, source.ID, "backup-1")
	assert.Equal("imported", imported.Status, "first source import status")
	failed := getDriveSourceImportItem(t, f.Store, source.ID, "backup-2")
	assert.Equal("failed", failed.Status, "second source import status")

	var unstamped int
	require.NoError(f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND embed_gen IS NULL`),
		source.ID,
	).Scan(&unstamped), "count unstamped messages")
	assert.Equal(2, unstamped, "imported messages remain discoverable by scan-and-fill")
}

func TestRunConfiguredSynctechSMSSourceLeavesManualSyncMessagesUnstamped(t *testing.T) {
	stubScheduledCacheBuild(t)
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	home := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() {
		cfg = savedCfg
	})
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cfg.Vector.Enabled = true
	cfg.Vector.Embeddings.Endpoint = "http://127.0.0.1:1"
	cfg.Vector.Embeddings.Model = "fake"
	cfg.Vector.Embeddings.Dimension = 4

	importDir := filepath.Join(home, "synctech-local")
	require.NoError(os.MkdirAll(importDir, 0o700), "create import dir")
	require.NoError(os.WriteFile(filepath.Join(importDir, "messages.xml"), []byte(`<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="manual sync should enqueue" read="1" status="-1" contact_name="Alice" />
</smses>`), 0o600), "write backup")

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "InitSchema")
	require.NoError(st.Close(), "close store")

	src := config.SynctechSMSSource{
		Name:       "pixel-local",
		Backend:    "local",
		Path:       importDir,
		OwnerPhone: "+15550000001",
		IncludeSMS: true,
	}
	require.NoError(runConfiguredSynctechSMSSource(ctx, src), "runConfiguredSynctechSMSSource")

	st, err = store.Open(cfg.DatabaseDSN())
	require.NoError(err, "reopen store")
	t.Cleanup(func() { _ = st.Close() })
	source := getSynctechSource(t, st, src.OwnerPhone)
	var unstamped int
	require.NoError(st.DB().QueryRowContext(ctx,
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND embed_gen IS NULL`),
		source.ID,
	).Scan(&unstamped), "count unstamped messages")
	assert.Equal(1, unstamped, "manual sync message remains discoverable by scan-and-fill")
}

func TestConfiguredSynctechSMSCompletesAfterImport(t *testing.T) {
	stubScheduledCacheBuild(t)
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home

	f := storetest.New(t)
	xmlPath := filepath.Join(home, "sms.xml")
	require.NoError(os.WriteFile(xmlPath, []byte(`<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="hello from local" read="1" status="-1" contact_name="Alice" />
</smses>`), 0o600), "write sms fixture")
	src := synctechDriveTestSource()
	src.Backend = "local"
	src.Path = xmlPath

	err := runConfiguredSynctechSMSSourceWithStore(context.Background(), f.Store, src)

	require.NoError(err, "configured synctech-sms import")
	source := getSynctechSource(t, f.Store, src.OwnerPhone)
	run := getOnlySyncRun(t, f.Store, source.ID)
	assert.Equal(store.SyncStatusCompleted, run.Status, "sync status")
	assertSourceMessageCount(t, f.Store, source.ID, 1)
}

func stubScheduledCacheBuild(t *testing.T) {
	t.Helper()
	old := runBuildCacheSubprocess
	runBuildCacheSubprocess = func(context.Context, bool, bool) error { return nil }
	t.Cleanup(func() { runBuildCacheSubprocess = old })
}

type fakeSynctechDriveClient struct {
	files           []synctechsms.DriveFile
	downloads       map[string]string
	listErr         error
	downloadErr     error
	downloadErrByID map[string]error
}

func (f fakeSynctechDriveClient) ListBackupFiles(context.Context, string) ([]synctechsms.DriveFile, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.files, nil
}

func (f fakeSynctechDriveClient) DownloadToFile(_ context.Context, fileID, path string) error {
	if err := f.downloadErrByID[fileID]; err != nil {
		return err
	}
	if f.downloadErr != nil {
		return f.downloadErr
	}
	return os.WriteFile(path, []byte(f.downloads[fileID]), 0o600)
}

func synctechDriveTestSource() config.SynctechSMSSource {
	return config.SynctechSMSSource{
		Name:               "pixel",
		Enabled:            true,
		Backend:            "drive",
		FolderID:           "drive-folder-id",
		GoogleAccount:      "user@example.com",
		OwnerPhone:         "+15550000001",
		StableAfter:        "10m",
		IncludeSMS:         true,
		IncludeMMS:         true,
		IncludeCalls:       true,
		IncludeAttachments: true,
	}
}

func getSynctechSource(t *testing.T, st *store.Store, ownerPhone string) *store.Source {
	t.Helper()
	sources, err := st.ListSources(synctechsms.SourceType)
	require.NoError(t, err, "ListSources")
	for _, source := range sources {
		if source.Identifier == ownerPhone {
			return source
		}
	}
	require.Failf(t, "synctech source not found", "owner_phone=%s sources=%#v", ownerPhone, sources)
	return nil
}

func countSyncRuns(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var got int
	err := st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id = ?`), sourceID).Scan(&got)
	require.NoError(t, err, "count sync runs")
	return got
}

func getOnlySyncRun(t *testing.T, st *store.Store, sourceID int64) store.SyncRun {
	t.Helper()
	var run store.SyncRun
	err := st.DB().QueryRow(st.Rebind(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after
		FROM sync_runs
		WHERE source_id = ?
	`), sourceID).Scan(
		&run.ID, &run.SourceID, &run.StartedAt, &run.CompletedAt, &run.Status,
		&run.MessagesProcessed, &run.MessagesAdded, &run.MessagesUpdated, &run.ErrorsCount,
		&run.ErrorMessage, &run.CursorBefore, &run.CursorAfter,
	)
	require.NoError(t, err, "get sync run")
	return run
}

func getDriveSourceImportItem(t *testing.T, st *store.Store, sourceID int64, providerID string) *store.SourceImportItem {
	t.Helper()
	item, err := st.GetSourceImportItem(sourceID, "drive", providerID)
	require.NoError(t, err, "GetSourceImportItem")
	return item
}

func assertSourceMessageCount(t *testing.T, st *store.Store, sourceID int64, want int) {
	t.Helper()
	var got int
	err := st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), sourceID).Scan(&got)
	require.NoError(t, err, "count source messages")
	assert.Equal(t, want, got, "source message count")
}

func assertSourceConversationMessageCount(t *testing.T, st *store.Store, sourceID int64, want int) {
	t.Helper()
	var got int
	err := st.DB().QueryRow(st.Rebind(`SELECT COALESCE(MAX(message_count), 0) FROM conversations WHERE source_id = ?`), sourceID).Scan(&got)
	require.NoError(t, err, "read conversation message_count")
	assert.Equal(t, want, got, "conversation message_count")
}
