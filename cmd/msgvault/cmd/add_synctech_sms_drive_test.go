package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAddSynctechSMSDriveWritesConfigWithoutSecrets(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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

	err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client)
	require.NoError(err, "runSynctechSMSDriveSourceWithClient")

	source := getSynctechSource(t, f.Store, src.OwnerPhone)
	assert.Equal(1, countSyncRuns(t, f.Store, source.ID), "sync run count")
	run := getOnlySyncRun(t, f.Store, source.ID)
	assert.Equal(store.SyncStatusCompleted, run.Status, "sync status")
	assert.Equal(int64(1), run.MessagesProcessed, "messages processed")
	assert.Equal(int64(1), run.MessagesAdded, "messages added")
	assert.True(getSynctechSource(t, f.Store, src.OwnerPhone).LastSyncAt.Valid, "last_sync_at should be touched")

	item := getSourceImportItem(t, f.Store, source.ID, "drive", "backup-1")
	assert.Equal("imported", item.Status, "source import status")
	assert.Equal(1, item.RecordsImported, "records imported")
	assert.False(item.ErrorMessage.Valid, "source import error")
	assertSourceMessageCount(t, f.Store, source.ID, 1)
	assertSourceConversationMessageCount(t, f.Store, source.ID, 1)
}

func TestSynctechSMSDriveRunRecordsZeroSelectedPoll(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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

	err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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

	err := runSynctechSMSDriveSourceWithClient(context.Background(), f.Store, src, synctechImportOptions(src), client)
	require.ErrorIs(err, downloadErr, "runSynctechSMSDriveSourceWithClient")

	source := getSynctechSource(t, f.Store, src.OwnerPhone)
	assert.Equal(1, countSyncRuns(t, f.Store, source.ID), "sync run count")
	run := getOnlySyncRun(t, f.Store, source.ID)
	assert.Equal(store.SyncStatusFailed, run.Status, "sync status")
	require.True(run.ErrorMessage.Valid, "sync error_message")
	assert.Contains(run.ErrorMessage.String, downloadErr.Error(), "sync error_message")

	item := getSourceImportItem(t, f.Store, source.ID, "drive", "backup-1")
	assert.Equal("failed", item.Status, "source import status")
	require.True(item.ErrorMessage.Valid, "source import error")
	assert.Contains(item.ErrorMessage.String, downloadErr.Error(), "source import error")
}

type fakeSynctechDriveClient struct {
	files       []synctechsms.DriveFile
	downloads   map[string]string
	listErr     error
	downloadErr error
}

func (f fakeSynctechDriveClient) ListBackupFiles(context.Context, string) ([]synctechsms.DriveFile, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.files, nil
}

func (f fakeSynctechDriveClient) DownloadToFile(_ context.Context, fileID, path string) error {
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
	requirepkg.NoError(t, err, "ListSources")
	for _, source := range sources {
		if source.Identifier == ownerPhone {
			return source
		}
	}
	requirepkg.Failf(t, "synctech source not found", "owner_phone=%s sources=%#v", ownerPhone, sources)
	return nil
}

func countSyncRuns(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var got int
	err := st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id = ?`), sourceID).Scan(&got)
	requirepkg.NoError(t, err, "count sync runs")
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
	requirepkg.NoError(t, err, "get sync run")
	return run
}

func getSourceImportItem(t *testing.T, st *store.Store, sourceID int64, provider, providerID string) *store.SourceImportItem {
	t.Helper()
	item, err := st.GetSourceImportItem(sourceID, provider, providerID)
	requirepkg.NoError(t, err, "GetSourceImportItem")
	return item
}

func assertSourceMessageCount(t *testing.T, st *store.Store, sourceID int64, want int) {
	t.Helper()
	var got int
	err := st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), sourceID).Scan(&got)
	requirepkg.NoError(t, err, "count source messages")
	assertpkg.Equal(t, want, got, "source message count")
}

func assertSourceConversationMessageCount(t *testing.T, st *store.Store, sourceID int64, want int) {
	t.Helper()
	var got int
	err := st.DB().QueryRow(st.Rebind(`SELECT COALESCE(MAX(message_count), 0) FROM conversations WHERE source_id = ?`), sourceID).Scan(&got)
	requirepkg.NoError(t, err, "read conversation message_count")
	assertpkg.Equal(t, want, got, "conversation message_count")
}
