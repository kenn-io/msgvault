package cmd

import (
	"bytes"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestGCAlwaysProxiesThroughDaemonCLIRunner(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal(t, []string{"gc", "--no-backup", "--yes"}, req.Args)
		},
		`{"type":"stdout","data":"Deleted 2 source-deleted message(s).\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	var stdout bytes.Buffer
	cmd := newGCCmd()
	cmd.SetArgs([]string{"--yes", "--no-backup"})
	cmd.SetOut(&stdout)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, 1, int(requests.Load()))
	assert.Contains(t, stdout.String(), "Deleted 2 source-deleted message(s).")
}

func TestGCConfirmsBeforeProxyingToDaemon(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal(t, []string{"gc", "--no-backup", "--yes"}, req.Args)
		},
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	var stdout bytes.Buffer
	cmd := newGCCmd()
	cmd.SetArgs([]string{"--no-backup"})
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetOut(&stdout)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, 1, int(requests.Load()))
	assert.Contains(t, stdout.String(), "Proceed? This is irreversible.")
}

func TestGCCancelledConfirmationDoesNotProxy(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	var stdout bytes.Buffer
	cmd := newGCCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetOut(&stdout)

	require.NoError(t, cmd.Execute())
	assert.Zero(t, int(requests.Load()))
	assert.Contains(t, stdout.String(), "Aborted.")
}

func TestRunGCLocalCancellationWritesNothing(t *testing.T) {
	assert := assert.New(t)
	deletedID, _, _ := seedGCCommandArchive(t)
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetOut(&output)

	require.NoError(t, runGCLocal(cmd, gcOptions{noBackup: true}))
	assert.Contains(output.String(), "Source-deleted messages to purge: 1")
	assert.Contains(output.String(), "Dedup-hidden messages retained: 1")
	assert.Contains(output.String(), "Aborted.")
	assert.True(gcMessageExists(t, cfg.DatabaseDSN(), deletedID),
		"cancelled GC must preserve the source-deleted row")
}

func TestRunGCLocalBacksUpBeforeDeleteAndCompacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	deletedID, activeID, dedupID := seedGCCommandArchive(t)
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&output)

	orphanBlob := seedGCLooseBlob(t, deletedID, strings.Repeat("0a", 32))
	sharedBlob := seedGCLooseBlob(t, deletedID, strings.Repeat("0b", 32))
	seedGCAttachmentRow(t, activeID, strings.Repeat("0b", 32))

	require.NoError(runGCLocal(cmd, gcOptions{yes: true}))
	assert.Contains(output.String(), "Source-deleted messages to purge: 1")
	assert.Contains(output.String(), "Dedup-hidden messages retained: 1")
	assert.Contains(output.String(), "Deleted 1 source-deleted message(s).")
	assert.Contains(output.String(), "SQLite archive compacted.")
	assert.Contains(output.String(), "Removed 1 unreferenced loose attachment blob(s).")
	assert.False(gcMessageExists(t, cfg.DatabaseDSN(), deletedID))
	assert.True(gcMessageExists(t, cfg.DatabaseDSN(), activeID))
	assert.True(gcMessageExists(t, cfg.DatabaseDSN(), dedupID),
		"dedup-only row must survive GC")
	assert.NoFileExists(orphanBlob,
		"a blob referenced only by the purged message must be swept")
	assert.FileExists(sharedBlob,
		"a blob still referenced by an active message must survive")

	backups, err := filepath.Glob(cfg.DatabaseDSN() + ".gc-backup-*")
	require.NoError(err, "glob GC backups")
	require.Len(backups, 1, "default GC must create one backup")
	assert.True(gcMessageExists(t, backups[0], deletedID),
		"backup must capture the source-deleted row before purge")

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err, "open compacted archive")
	defer func() { _ = st.Close() }()
	var freelist int64
	require.NoError(st.DB().QueryRow(`PRAGMA freelist_count`).Scan(&freelist),
		"read compacted freelist")
	assert.Zero(freelist)
}

func TestRunGCLocalBacksUpFileURIDatabaseBesideArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	deletedID, _, _ := seedGCCommandArchive(t)
	dbPath := cfg.DatabaseDSN()
	cfg.Data.DatabaseURL = (&url.URL{
		Scheme: "file",
		Path:   dbPath,
	}).String()

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&output)

	require.NoError(runGCLocal(cmd, gcOptions{yes: true}))

	backups, err := filepath.Glob(dbPath + ".gc-backup-*")
	require.NoError(err, "glob GC backups")
	require.Len(backups, 1, "backup should use the decoded database path")
	assert.True(gcMessageExists(t, backups[0], deletedID),
		"backup must capture the source-deleted row before purge")
	assert.NotContains(filepath.Base(backups[0]), "?")
}

func seedGCCommandArchive(t *testing.T) (deletedID, activeID, dedupID int64) {
	t.Helper()
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	t.Cleanup(func() { cfg = savedCfg })

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err, "OpenForTest")
	require.NoError(st.InitSchema(), "InitSchema")
	source, err := st.GetOrCreateSource("gmail", "gc@example.test")
	require.NoError(err, "GetOrCreateSource")
	conversationID, err := st.EnsureConversation(source.ID, "gc-thread", "GC")
	require.NoError(err, "EnsureConversation")
	insert := func(sourceMessageID string) int64 {
		id, insertErr := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: sourceMessageID,
			MessageType:     store.MessageTypeEmail,
		})
		require.NoError(insertErr, "UpsertMessage %s", sourceMessageID)
		return id
	}
	deletedID = insert("gc-command-deleted")
	activeID = insert("gc-command-active")
	dedupID = insert("gc-command-dedup")
	require.NoError(st.MarkMessageDeleted(source.ID, "gc-command-deleted"),
		"MarkMessageDeleted")
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages
		SET deleted_at = CURRENT_TIMESTAMP, delete_batch_id = 'gc-command-batch'
		WHERE id = ?
	`), dedupID)
	require.NoError(err, "mark dedup-only row")
	require.NoError(st.UpsertMessageBody(deletedID,
		sql.NullString{String: strings.Repeat("deleted payload ", 100_000), Valid: true},
		sql.NullString{}), "store deleted payload")
	require.NoError(st.Close(), "close seed store")
	return deletedID, activeID, dedupID
}

// seedGCLooseBlob writes a loose content-addressed blob file and attaches it
// to messageID, returning the blob's filesystem path.
func seedGCLooseBlob(t *testing.T, messageID int64, hash string) string {
	t.Helper()
	seedGCAttachmentRow(t, messageID, hash)
	fullPath := filepath.Join(
		cfg.AttachmentsDir(), hash[:2], hash,
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755), "create blob dir")
	require.NoError(t, os.WriteFile(fullPath, []byte("gc blob "+hash), 0o600),
		"write loose blob")
	return fullPath
}

func seedGCAttachmentRow(t *testing.T, messageID int64, hash string) {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err, "open store for attachment seed")
	defer func() { _ = st.Close() }()
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO attachments (message_id, filename, content_hash, storage_path)
		VALUES (?, ?, ?, ?)
	`), messageID, "blob.bin", hash, hash[:2]+"/"+hash)
	require.NoError(t, err, "insert attachment row")
}

func gcMessageExists(t *testing.T, dbPath string, messageID int64) bool {
	t.Helper()
	st, err := store.OpenForTest(dbPath)
	require.NoError(t, err, "open GC archive")
	defer func() { _ = st.Close() }()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE id = ?`), messageID).Scan(&count),
		"count GC message")
	return count == 1
}
