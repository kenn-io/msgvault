package signalandroid

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestFailedWriteCounts(t *testing.T) {
	assert := assert.New(t)
	st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_carol BEFORE INSERT ON messages WHEN NEW.snippet = 'Carol here' BEGIN SELECT RAISE(ABORT, 'message failure'); END`)
	require.NoError(t, err)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(int64(1), summary.Errors)
	assert.Equal(int64(1), summary.AttachmentsFound)
	assert.Zero(summary.AttachmentsMissing)
	assert.Equal(int64(1), summary.AttachmentsStored)
}

func TestRerunCountsUnchanged(t *testing.T) {
	for _, name := range []string{"unreadable source", "missing source keeps occurrence"} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			occurrence := name == "missing source keeps occurrence"
			if occurrence {
				changeFixture(t, dir, `UPDATE attachment SET content_type = 'application/pdf', file_name = 'example.pdf' WHERE _id = 500`)
			}
			storage := t.TempDir()
			runImport(t, st, dir, storage)
			var attachmentID int64
			if occurrence {
				require.NoError(st.DB().QueryRow(`SELECT id FROM attachments`).Scan(&attachmentID))
				_, eligible, err := st.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
				require.NoError(err)
				require.True(eligible)
			}
			file := filepath.Join(dir, "Attachment_500_-1.bin")
			require.NoError(os.Remove(file))
			if !occurrence {
				require.NoError(os.Mkdir(file, 0o700))
			}
			summary := runImport(t, st, dir, storage)
			if occurrence {
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE id = ?`, attachmentID))
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM document_occurrences WHERE attachment_id = ?`, attachmentID))
			} else {
				assert.Zero(summary.ReactionsAdded)
				assert.Zero(summary.AttachmentsStored)
				assert.Zero(summary.Errors)
			}
		})
	}
}

func TestMigratedIncomingOnly(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.V185, 0)
	changeFixture(t, dir, `DELETE FROM message WHERE (type & 31) IN (21, 22, 23, 24)`, `UPDATE message SET to_recipient_id = from_recipient_id WHERE thread_id IN (10, 12)`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(4), summary.MessagesAdded)
}

func TestInterruptedHistoricalSender(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `DELETE FROM message WHERE thread_id <> 11`, `DELETE FROM group_membership WHERE recipient_id = 3`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone, opts.BatchSize = signalfixture.OwnPhone, 1
	summary, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(err, context.Canceled)
	require.NotNil(summary)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages`))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversation_participants cp JOIN messages m ON m.conversation_id = cp.conversation_id AND m.sender_id = cp.participant_id`))
}

func TestImportPrefersContactNickname(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE recipient SET nickname_joined_name = 'Example nickname' WHERE _id = 2`)
	runImport(t, st, dir, t.TempDir())
	assert.Equal(t, "Hi @Example nickname, look\n\nBeach", importedMessages(t, st)["signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)].body)
}

func TestCurrentDeletedMessageIsSkipped(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET deleted_by = from_recipient_id, remote_deleted = 0 WHERE _id = 300`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(5), summary.MessagesAdded)
	assert.Equal(t, 0, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE snippet = 'Carol here'`))
}

func TestReactorConflictLeavesSnapshot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	before := importedMessages(t, st)
	// A new phone for an ACI-only reactor conflicts with two curated people.
	aciPID, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.BobACI)
	require.NoError(err)
	_, _, err = st.CreatePersonFromParticipant(aciPID)
	require.NoError(err)
	phonePID, err := st.EnsureParticipantByPhone("+15555550102", "Other Example", SourceType)
	require.NoError(err)
	_, _, err = st.CreatePersonFromParticipant(phonePID)
	require.NoError(err)
	// Carol's sender resolves normally; only her reactor has a conflict.
	changeFixture(t, dir, `UPDATE recipient SET e164 = '+15555550102' WHERE _id = 3`, `UPDATE message SET body = 'Changed text' WHERE _id = 300`, `INSERT INTO reaction (message_id, author_id, emoji, date_sent, date_received) VALUES (300, 3, 'x', 1700000300100, 1700000300101)`)
	summary := runImport(t, st, dir, storage)
	require.Positive(summary.Errors)
	assert.Equal(before, importedMessages(t, st))
	assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Changed'`))
}
