package signalandroid

import (
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
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE attachment SET content_type = 'application/pdf', file_name = 'example.pdf' WHERE _id = 500`)
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	var attachmentID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM attachments`).Scan(&attachmentID))
	_, eligible, err := st.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
	require.NoError(err)
	require.True(eligible)
	require.NoError(os.Remove(filepath.Join(dir, "Attachment_500_-1.bin")))
	summary := runImport(t, st, dir, storage)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	assert.Zero(summary.ReactionsAdded)
	assert.Zero(summary.AttachmentsStored)
	assert.Zero(summary.Errors)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE id = ?`, attachmentID))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM document_occurrences WHERE attachment_id = ?`, attachmentID))
}

func TestMigratedIncomingOnly(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.V185, 0)
	changeFixture(t, dir, `DELETE FROM message WHERE (type & 31) IN (21, 22, 23, 24)`, `UPDATE message SET to_recipient_id = from_recipient_id WHERE thread_id IN (10, 12)`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(5), summary.MessagesAdded)
}

func TestImportPrefersContactNickname(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE recipient SET nickname_joined_name = 'Example nickname' WHERE _id = 2`)
	runImport(t, st, dir, t.TempDir())
	assert.Equal(t, "Hi @Example nickname, look\n\nBeach", importedMessages(t, st)["signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)].body)
}

func TestImportDirectStoryReply(t *testing.T) {
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET parent_story_id = -9 WHERE _id = 100`)
	runImport(t, st, dir, t.TempDir())
	id := "signal:aci:" + signalfixture.AliceACI + ":peer:" + ms(signalfixture.TAlice1)
	message, ok := importedMessages(t, st)[id]
	require.True(t, ok)
	assert.Equal(t, "Hello from Alice", message.body)
	assert.Equal(t, "signal:aci:"+signalfixture.AliceACI, message.conversation)
}

func TestCurrentDeletedMessageIsSkipped(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET deleted_by = from_recipient_id, remote_deleted = 0 WHERE _id = 300`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(6), summary.MessagesAdded)
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
