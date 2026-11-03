package signalandroid

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestMissingLongTextOnRerun(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	full := strings.Repeat("long text ", 300) + "searchablesuffix"
	changeFixture(t, dir, `UPDATE message SET body = 'preview' WHERE _id = 300`, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
	file := filepath.Join(dir, "Attachment_503_-1.bin")
	require.NoError(t, os.WriteFile(file, []byte(full), 0o600))
	runImport(t, st, dir, t.TempDir())
	require.NoError(t, os.Remove(file))
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, full, importedMessages(t, st)["signal:aci:"+signalfixture.CarolACI+":"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)].body)
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchablesuffix'`))
	assert.Equal(t, int64(2), summary.AttachmentsMissing)
}

func TestMissingLongTextOnFirstImport(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(2), summary.AttachmentsMissing)
	assert.Equal(t, "Carol here", importedMessages(t, st)["signal:aci:"+signalfixture.CarolACI+":"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)].body)
}

func TestUnavailableAttachmentKeepsMessage(t *testing.T) {
	for _, storageFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "oversize", true: "storage failure"}[storageFailure], func(t *testing.T) {
			st := testutil.NewTestStore(t)
			dir := t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, t.TempDir()
			if storageFailure {
				opts.AttachmentsDir = filepath.Join(t.TempDir(), "file")
				require.NoError(t, os.WriteFile(opts.AttachmentsDir, []byte("file"), 0o600))
			} else {
				opts.MaxAttachmentSize = 3
			}
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(t, err)
			assert.Equal(t, expectedMessages(), importedMessages(t, st))
			assert.Equal(t, int64(2), summary.AttachmentsMissing)
		})
	}
}

func TestProducerVersionSidecar(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		wantError  bool
	}{
		{"supported", "VERSION:uint32:330\n", false},
		{"future", "VERSION:uint32:331\n", true},
		{"old", "VERSION:uint32:184\n", true},
		{"malformed", "VERSION:uint32:garbage\n", true},
		{"missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			dir := t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `PRAGMA user_version = 0`)
			versionPath := filepath.Join(dir, "DatabaseVersion.sbf")
			if tc.data != "" {
				require.NoError(t, os.WriteFile(versionPath, []byte(tc.data), 0o600))
			} else {
				_ = os.Remove(versionPath)
			}
			opts := DefaultOptions()
			opts.Phone = signalfixture.OwnPhone
			_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			if tc.wantError {
				require.ErrorIs(t, err, ErrUnsupportedDatabase)
				if tc.name == "future" {
					assert.Contains(t, err.Error(), "331")
				}
				assert.Zero(t, countRows(t, st, `SELECT COUNT(*) FROM sources`))
			} else {
				require.NoError(t, err)
				assert.Len(t, importedMessages(t, st), 6)
			}
		})
	}
}

func TestGroupOnlyBackup(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `DELETE FROM message WHERE thread_id <> 11`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(1), summary.MessagesAdded)
}

func TestPhoneAppearsForArchivedSender(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())
	changeFixture(t, dir, `DELETE FROM message WHERE _id = 200`, `UPDATE recipient SET e164 = '+15555550102' WHERE _id = 3`)
	runImport(t, st, dir, t.TempDir())
	pid, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.BobACI)
	require.NoError(t, err)
	var sender int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE source_message_id = ?`), "signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)).Scan(&sender))
	assert.Equal(t, pid, sender)
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM conversation_participants cp JOIN participants p ON p.id = cp.participant_id WHERE p.id = ?`, pid))
	assert.Equal(t, 0, countRows(t, st, `SELECT COUNT(*) FROM participants WHERE id = ? AND phone_number IS NULL`, sender))
}

func TestInterruptedReplyIsComplete(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone, opts.BatchSize = signalfixture.OwnPhone, 2
	_, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL`))
}

func TestDeferredReplyAfterBackupDropsChild(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET _id = 107 WHERE _id = 100`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone, opts.BatchSize = signalfixture.OwnPhone, 1
	_, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(t, err, context.Canceled)
	changeFixture(t, dir, `DELETE FROM message WHERE _id = 101`)
	runImport(t, st, dir, t.TempDir())
	var parent string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?`), "signal:aci:"+signalfixture.AliceACI+":"+signalfixture.OwnACI+":"+ms(signalfixture.TReply)).Scan(&parent))
	assert.Equal(t, "signal:aci:"+signalfixture.AliceACI+":"+signalfixture.AliceACI+":"+ms(signalfixture.TAlice1), parent)
}

func TestStatsFailureCannotCompleteImport(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_stats BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(ABORT, 'stats failure'); END`)
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.Phone = signalfixture.OwnPhone
	_, err = NewImporter(st, nil).Import(t.Context(), path, opts)
	require.ErrorContains(t, err, "stats failure")
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM sync_runs WHERE status = 'failed'`))
}

func TestImportCountsSkippedMessages(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(10), summary.MessagesProcessed)
	assert.Equal(t, int64(4), summary.MessagesSkipped)
}

func TestNegativeLimit(t *testing.T) {
	opts := DefaultOptions()
	opts.Phone, opts.Limit = signalfixture.OwnPhone, -1
	_, err := NewImporter(testutil.NewTestStore(t), nil).Import(t.Context(), "missing.sqlite", opts)
	require.ErrorContains(t, err, "limit must be non-negative")
}
