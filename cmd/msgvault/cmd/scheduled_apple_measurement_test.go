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

// setWhatsAppWriter makes the WhatsApp process probe report alive (nil for
// unknown) for the rest of the test.
func setWhatsAppWriter(t *testing.T, alive *bool) {
	t.Helper()
	old := whatsAppWriterAlive
	whatsAppWriterAlive = func() *bool { return alive }
	t.Cleanup(func() { whatsAppWriterAlive = old })
}

func TestScheduledWhatsAppAppleWriterNotRunningIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newWhatsAppAppleFixture(t)
	ctx := scheduledAppleContext(t)
	src := config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path}
	closed := false
	setWhatsAppWriter(t, &closed)

	// The first run imports the fixture message, so it read the source even
	// though WhatsApp is closed now.
	require.NoError(runScheduledWhatsAppApple(ctx, st, src))
	run, m := latestMeasurement(t, st, "whatsapp")
	assert.Equal(int64(1), run.MessagesAdded)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeCompleted, m.Outcome)
	source, err := st.GetSourceByID(run.SourceID)
	require.NoError(err)
	require.True(source.LastSyncAt.Valid)
	freshness := source.LastSyncAt.Time

	// The second run finds nothing new while WhatsApp is closed.
	require.NoError(runScheduledWhatsAppApple(ctx, st, src))
	run, m = latestMeasurement(t, st, "whatsapp")
	assert.Equal(store.SyncStatusCompleted, run.Status)
	assert.Zero(run.MessagesAdded)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonWriterNotRunning, m.Reason)
	require.NotNil(m.WriterAlive)
	assert.False(*m.WriterAlive)
	source, err = st.GetSourceByID(run.SourceID)
	require.NoError(err)
	assert.True(freshness.Equal(source.LastSyncAt.Time), "an unmeasured run must not advance freshness")
}

func TestScheduledWhatsAppAppleWriterRunningIsCompleted(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newWhatsAppAppleFixture(t)
	ctx := scheduledAppleContext(t)
	src := config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path}
	running := true
	setWhatsAppWriter(t, &running)

	require.NoError(runScheduledWhatsAppApple(ctx, st, src))
	require.NoError(runScheduledWhatsAppApple(ctx, st, src))

	run, m := latestMeasurement(t, st, "whatsapp")
	assert.Zero(run.MessagesAdded)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeCompleted, m.Outcome)
	assert.Empty(m.Reason)
	assert.NotNil(m.ReadStartedAt)
}

func TestScheduledWhatsAppAppleUnknownWriterIsCompleted(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newWhatsAppAppleFixture(t)
	ctx := scheduledAppleContext(t)
	src := config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path}
	setWhatsAppWriter(t, nil)

	require.NoError(runScheduledWhatsAppApple(ctx, st, src))
	require.NoError(runScheduledWhatsAppApple(ctx, st, src))

	_, m := latestMeasurement(t, st, "whatsapp")
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeCompleted, m.Outcome)
	assert.Nil(m.WriterAlive)
}

func TestWhatsAppWriterAliveUnknownOffMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS probes the real WhatsApp process")
	}
	assert.Nil(t, whatsAppWriterAlive(), "a process check off macOS says nothing about WhatsApp for Mac")
}

// TestScheduledWhatsAppAppleRemovedAfterPreflightIsUnmeasured proves a source
// removed between the readability probe and importer open supersedes a prior
// completed status with a source_missing observation.
func TestScheduledWhatsAppAppleRemovedAfterPreflightIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newWhatsAppAppleFixture(t)
	ctx := scheduledAppleContext(t)
	src := config.WhatsAppAppleSource{Name: "test", Phone: "+15555550100", Path: path}
	require.NoError(runScheduledWhatsAppApple(ctx, st, src))

	old := checkAppleSourceReadable
	checkAppleSourceReadable = func(path string) error {
		err := old(path)
		if err == nil {
			require.NoError(os.Remove(path))
		}
		return err
	}
	t.Cleanup(func() { checkAppleSourceReadable = old })

	require.Error(runScheduledWhatsAppApple(ctx, st, src))
	run, m := latestMeasurement(t, st, "whatsapp")
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonSourceMissing, m.Reason)
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

func TestOpenImessageClientRecordedRemovedAfterCheckIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	path := newImessageFixture(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)

	old := checkAppleSourceReadable
	checkAppleSourceReadable = func(path string) error {
		err := old(path)
		if err == nil {
			require.NoError(os.Remove(path))
		}
		return err
	}
	t.Cleanup(func() { checkAppleSourceReadable = old })

	_, err = openImessageClientRecorded(t.Context(), st, src, path, nil, time.Now())
	require.Error(err)

	run, m := latestMeasurement(t, st, "apple_messages")
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonSourceMissing, m.Reason)
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

	require.Error(t, checkAppleSourceReadable(path))
	assert.Equal(t, store.SyncReasonFDADenied, sourceUnreadableReason(path))
}

// runManualImessageImport runs the import-imessage command against chatDB in
// a fresh archive and returns that archive's store. seed runs on the store
// before the command.
func runManualImessageImport(t *testing.T, chatDB string, seed func(*store.Store)) (*store.Store, error) {
	t.Helper()
	markDaemonCLISubprocessForTest(t)
	cfg := config.NewDefaultConfig()
	dataDir := t.TempDir()
	cfg.HomeDir = dataDir
	cfg.Data.DataDir = dataDir
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	if seed != nil {
		seed(st)
	}
	savedPath := importImessageDBPath
	importImessageDBPath = chatDB
	t.Cleanup(func() { importImessageDBPath = savedPath })

	command := &cobra.Command{Use: "import-imessage"}
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	return st, runImportImessage(command, nil)
}

// TestManualImessageImportMissingSourceCreatesNoSource covers a manual import
// on a machine without Messages: it reports the missing chat.db and leaves no
// source behind.
func TestManualImessageImportMissingSourceCreatesNoSource(t *testing.T) {
	require := require.New(t)
	missing := filepath.Join(t.TempDir(), "chat.db")

	st, err := runManualImessageImport(t, missing, nil)

	require.ErrorContains(err, "iMessage database not found at "+missing)
	require.ErrorIs(err, fs.ErrNotExist)
	sources, err := st.ListSources("apple_messages")
	require.NoError(err)
	assert.Empty(t, sources)
}

// TestManualImessageImportMissingSourceIsUnmeasured covers a manual import of
// an archived iMessage source whose chat.db is gone: the run is recorded as
// unmeasured source_missing.
func TestManualImessageImportMissingSourceIsUnmeasured(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	missing := filepath.Join(t.TempDir(), "chat.db")

	st, err := runManualImessageImport(t, missing, func(st *store.Store) {
		_, err := resolveImessageSource(st)
		require.NoError(err)
	})

	require.ErrorIs(err, fs.ErrNotExist)
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
	path := newImessageFixture(t)
	src, err := resolveImessageSource(st)
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "imessage_import")
	require.NoError(err)
	denyAppleSource(t)

	failImessageRun(t.Context(), st, st.ScopedToSync(src.ID, syncID), syncID, path, time.Now(),
		errors.New("query chat.db: unable to open database file"))

	run, m := latestMeasurement(t, st, "apple_messages")
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.NotNil(m)
	assert.Equal(store.SyncOutcomeUnmeasured, m.Outcome)
	assert.Equal(store.SyncReasonFDADenied, m.Reason)
	assert.Contains(run.ErrorMessage.String, "unmeasured: fda_denied")
}

// TestImessageRunArchiveErrorStaysFailed covers failures on the archive side
// while chat.db stays readable, including a PostgreSQL privilege error whose
// text looks like a denied source: the run is a plain failure.
func TestImessageRunArchiveErrorStaysFailed(t *testing.T) {
	st := testutil.NewTestStore(t)
	path := newImessageFixture(t)
	src, err := resolveImessageSource(st)
	require.NoError(t, err)

	for _, cause := range []string{"disk is full", "pq: permission denied for table messages"} {
		t.Run(cause, func(t *testing.T) {
			syncID, err := st.StartSync(src.ID, "imessage_import")
			require.NoError(t, err)

			assert := assert.New(t)
			failImessageRun(t.Context(), st, st.ScopedToSync(src.ID, syncID), syncID, path, time.Now(), errors.New(cause))

			run, m := latestMeasurement(t, st, "apple_messages")
			assert.Equal(store.SyncStatusFailed, run.Status)
			assert.Equal(cause, run.ErrorMessage.String)
			assert.Nil(m, "an archive failure records no measurement")
		})
	}
}

// Preflight failures never enter an importer; failures after import begins must
// still invalidate the cache even when no checkpoint counters were saved.
func TestUnmeasuredSyncCacheInterruptionCounters(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("apple_messages", "cache@example.test")
	require.NoError(err)
	before, err := readCacheSyncCounters(st.DB())
	require.NoError(err)
	for _, reason := range []string{store.SyncReasonSourceMissing, store.SyncReasonFDADenied} {
		require.NoError(st.RecordUnmeasuredSync(ctx, src.ID, "test", reason, "unavailable", store.SyncMeasurement{}))
		after, err := readCacheSyncCounters(st.DB())
		require.NoError(err)
		assert.Equal(before, after)
	}
	id, err := st.StartSync(src.ID, "test")
	require.NoError(err)
	// A failed importer can have committed writes before its next checkpoint.
	conversationID, err := st.EnsureConversation(src.ID, "partial", "Partial import")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{ConversationID: conversationID, SourceID: src.ID, SourceMessageID: "partial", MessageType: "sms"})
	require.NoError(err)
	require.NoError(st.FailSync(id, store.UnmeasuredSyncError(store.SyncReasonFDADenied, "lost access during import")))
	after, err := readCacheSyncCounters(st.DB())
	require.NoError(err)
	assert.Equal(before.failedRunCount+1, after.failedRunCount)
	assert.Equal(before.failedRunIDSum+id, after.failedRunIDSum)
}
