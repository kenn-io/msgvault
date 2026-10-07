package cmd

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newWhatsAppAppleFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(`
 CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT, ZPARTNERNAME TEXT, ZSESSIONTYPE INTEGER, ZLASTMESSAGEDATE TIMESTAMP);
 CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID TEXT, ZCONTACTNAME TEXT, ZFIRSTNAME TEXT, ZISADMIN INTEGER);
 CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZGROUPMEMBER INTEGER, ZSTANZAID TEXT, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZTEXT TEXT, ZMESSAGETYPE INTEGER, ZFROMJID TEXT);
 INSERT INTO ZWACHATSESSION VALUES (1, '15555550101@s.whatsapp.net', 'Alice Test', 0, 700000002);
 INSERT INTO ZWAMESSAGE VALUES (1, 1, NULL, 'direct-in', 0, 700000000.25, 'hello', 0, '15555550101@s.whatsapp.net');`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

func scheduledAppleContext(t *testing.T) context.Context {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	return testInvocationContext(t.Context(), cfg, invocationOptions{})
}

func latestMeasurement(t *testing.T, st *store.Store, sourceType string) (*store.SyncRun, *store.SyncMeasurement) {
	t.Helper()
	sources, err := st.ListSources(sourceType)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	run, err := st.GetLatestSyncContext(t.Context(), sources[0].ID, 0)
	require.NoError(t, err)
	m, err := st.GetSyncMeasurement(t.Context(), run.ID)
	if errors.Is(err, store.ErrSyncMeasurementNotFound) {
		return run, nil
	}
	require.NoError(t, err)
	return run, m
}

// denyAppleSource makes every Apple source open fail with a permission error,
// the way Full Disk Access does, without relying on file modes that not every
// OS enforces.
func denyAppleSource(t *testing.T) {
	t.Helper()
	old := checkAppleSourceReadable
	checkAppleSourceReadable = func(path string) error {
		return &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	}
	t.Cleanup(func() { checkAppleSourceReadable = old })
}

func TestScheduledIMessageUnreadableIsUnmeasuredNotZero(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newImessageFixture(t)
	denyAppleSource(t)

	err := runScheduledIMessage(scheduledAppleContext(t), st, config.IMessageConfig{DBPath: path})
	require.Error(err)

	run, m := latestMeasurement(t, st, "apple_messages")
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonFDADenied, m.Reason)
	assert.Zero(run.MessagesAdded)
}

func TestScheduledIMessageMissingSourceIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	missing := filepath.Join(t.TempDir(), "chat.db")

	require.Error(runScheduledIMessage(scheduledAppleContext(t), st, config.IMessageConfig{DBPath: missing}))

	_, m := latestMeasurement(t, st, "apple_messages")
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonSourceMissing, m.Reason)
}

func TestScheduledWhatsAppAppleWriterNotRunningIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newWhatsAppAppleFixture(t)
	old := whatsAppWriterAlive
	whatsAppWriterAlive = func() bool { return false }
	t.Cleanup(func() { whatsAppWriterAlive = old })

	err := runScheduledWhatsAppApple(scheduledAppleContext(t), st,
		config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path})
	require.NoError(err)

	run, m := latestMeasurement(t, st, "whatsapp")
	assert.Equal(store.SyncStatusCompleted, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonWriterNotRunning, m.Reason)
	require.NotNil(m.WriterAlive)
	assert.False(*m.WriterAlive)
}

func TestScheduledWhatsAppAppleWriterRunningIsCompleted(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newWhatsAppAppleFixture(t)
	old := whatsAppWriterAlive
	whatsAppWriterAlive = func() bool { return true }
	t.Cleanup(func() { whatsAppWriterAlive = old })

	require.NoError(runScheduledWhatsAppApple(scheduledAppleContext(t), st,
		config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path}))

	_, m := latestMeasurement(t, st, "whatsapp")
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeCompleted, m.Outcome)
	assert.Empty(m.Reason)
	assert.NotNil(m.ReadStartedAt)
}

func TestAppleSourceMtimeUsesWAL(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ChatStorage.sqlite")
	require.NoError(os.WriteFile(path, nil, 0o600))
	require.NoError(os.WriteFile(path+"-wal", nil, 0o600))
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	recent := time.Now().Add(-time.Minute).Truncate(time.Second)
	require.NoError(os.Chtimes(path, old, old))
	require.NoError(os.Chtimes(path+"-wal", recent, recent))

	got := appleSourceMtime(path)
	require.NotNil(got)
	assert.True(got.Equal(recent), "source_mtime %v should be the WAL mtime %v", got, recent)

	require.NoError(os.Remove(path + "-wal"))
	got = appleSourceMtime(path)
	require.NotNil(got)
	assert.True(got.Equal(old))
}

// TestOpenImessageClientRecordedDeniedIsUnmeasured covers the manual import
// path, which opens chat.db outside the scheduler: a denied source leaves an
// unmeasured fda_denied run, not nothing.
func TestOpenImessageClientRecordedDeniedIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newImessageFixture(t)
	denyAppleSource(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)

	_, err = openImessageClientRecorded(t.Context(), st, src, path, nil, time.Now())
	require.Error(err)

	run, m := latestMeasurement(t, st, "apple_messages")
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonFDADenied, m.Reason)
}

// TestCheckAppleSourceReadableDeniedByFileMode keeps the real file-mode path
// covered where the OS enforces modes.
func TestCheckAppleSourceReadableDeniedByFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not deny reads on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	path := newImessageFixture(t)
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	err := checkAppleSourceReadable(path)

	require.Error(t, err)
	assert.Equal(t, store.SyncReasonFDADenied, unmeasuredReason(err))
}

// TestManualImessageImportMissingSourceIsUnmeasured covers the manual import
// path with a chat.db that does not exist: the run is recorded as unmeasured
// source_missing instead of the command returning before any run is written.
func TestManualImessageImportMissingSourceIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	markDaemonCLISubprocessForTest(t)
	cfg := config.NewDefaultConfig()
	dataDir := t.TempDir()
	cfg.HomeDir = dataDir
	cfg.Data.DataDir = dataDir
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	missing := filepath.Join(t.TempDir(), "chat.db")
	savedPath := importImessageDBPath
	importImessageDBPath = missing
	t.Cleanup(func() { importImessageDBPath = savedPath })

	command := &cobra.Command{Use: "import-imessage"}
	command.SetContext(testCtx)
	err := runImportImessage(command, nil)

	require.Error(err)
	require.ErrorContains(err, "iMessage database not found at "+missing)
	require.ErrorIs(err, fs.ErrNotExist)
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	run, m := latestMeasurement(t, st, "apple_messages")
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonSourceMissing, m.Reason)
}

// TestImessageRunLosingAccessIsUnmeasured covers a source that stops being
// readable after the run started (Full Disk Access revoked mid-run): the run
// reports outcome=unmeasured, reason=fda_denied, as sources/status does for a
// source that could not be opened, instead of failed with the raw error.
func TestImessageRunLosingAccessIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "imessage_import")
	require.NoError(err)

	failImessageRun(t.Context(), st, st.ScopedToSync(src.ID, syncID), syncID, time.Now(),
		errors.New("connect to chat.db: unable to open database file: operation not permitted"))

	run, m := latestMeasurement(t, st, "apple_messages")
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonFDADenied, m.Reason)
	assert.Contains(run.ErrorMessage.String, "unmeasured: fda_denied")

	// A genuine failure stays a plain failure.
	failed, err := st.StartSync(src.ID, "imessage_import")
	require.NoError(err)
	failImessageRun(t.Context(), st, st.ScopedToSync(src.ID, failed), failed, time.Now(), errors.New("disk is full"))
	_, m = latestMeasurement(t, st, "apple_messages")
	assert.Nil(m, "an unclassified failure records no measurement")
}
