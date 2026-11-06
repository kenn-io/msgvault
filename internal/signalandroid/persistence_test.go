package signalandroid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestRerunAfterPackingKeepsMediaPacked(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir, storage := testutil.NewTestStore(t), t.TempDir(), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, storage)
	var hash, storagePath string
	require.NoError(st.DB().QueryRow(`SELECT content_hash, storage_path FROM attachments`).Scan(&hash, &storagePath))
	layout, err := packstore.NewLayout(storage, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	maintainer, err := packstore.NewMaintainer(store.NewPackCatalog(st), layout, packstore.MaintainerOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(maintainer.Close()) })
	stats, err := maintainer.Pack(t.Context(), packstore.PackOptions{})
	require.NoError(err)
	require.Equal(1, stats.BlobsPacked)
	require.NoFileExists(filepath.Join(storage, storagePath))

	summary := runImport(t, st, dir, storage)
	assert.Zero(summary.Errors)
	assert.Zero(summary.AttachmentsStored)
	assert.NoFileExists(filepath.Join(storage, storagePath))
	// Metadata changes also reuse the packed bytes.
	changeFixture(t, dir, `UPDATE attachment SET file_name = 'renamed.jpg' WHERE _id = 500`)
	summary = runImport(t, st, dir, storage)
	assert.Zero(summary.Errors)
	assert.Zero(summary.AttachmentsStored)
	assert.NoFileExists(filepath.Join(storage, storagePath))
	blobs, err := attachmentstore.New(store.NewPackCatalog(st), storage)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(blobs.Close()) })
	content, _, err := blobs.ReadBounded(hash, 100)
	require.NoError(err)
	assert.Equal("photo bytes", string(content))
}

func TestRerunRestoresMissingAttachmentFile(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir, storage := testutil.NewTestStore(t), t.TempDir(), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, storage)
	var attachmentID int64
	var storagePath string
	require.NoError(st.DB().QueryRow(`SELECT id, storage_path FROM attachments`).Scan(&attachmentID, &storagePath))
	path := filepath.Join(storage, storagePath)
	require.NoError(os.Remove(path))

	summary := runImport(t, st, dir, storage)
	assert.Zero(summary.Errors)
	assert.Equal(int64(1), summary.AttachmentsStored)
	content, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal("photo bytes", string(content))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE id = ?`, attachmentID))
	assert.Zero(runImport(t, st, dir, storage).AttachmentsStored)
}

func TestRerunCountsOnlyNewEdits(t *testing.T) {
	assert := assert.New(t)
	st, dir, storage := testutil.NewTestStore(t), t.TempDir(), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	assert.Equal(int64(1), runImport(t, st, dir, storage).MessagesEdited)
	assert.Zero(runImport(t, st, dir, storage).MessagesEdited)
	changeFixture(t, dir, `UPDATE message SET date_sent = date_sent + 1000, body = 'Another draft' WHERE _id = 106`)
	assert.Equal(int64(1), runImport(t, st, dir, storage).MessagesEdited)
	assert.Equal("Another draft", importedMessages(t, st)["signal:aci:"+signalfixture.AliceACI+":self:"+ms(signalfixture.TEditOld)].body)
	assert.Zero(runImport(t, st, dir, storage).MessagesEdited)
}

func TestFailedWriteCounts(t *testing.T) {
	assert := assert.New(t)
	st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_carol BEFORE INSERT ON messages WHEN NEW.snippet = 'Carol here' BEGIN SELECT RAISE(ABORT, 'message failure'); END`)
	require.NoError(t, err)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(int64(1), summary.Errors)
	assert.Equal(int64(1), summary.AttachmentsFound)
	assert.Zero(summary.AttachmentsMissing)
	assert.Equal(int64(1), summary.AttachmentsStored)
}

func TestRerunAfterGroupMemberWriteFailureRestoresRecipients(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir, storage := testutil.NewSQLiteTestStore(t), t.TempDir(), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	// Alice is only a group member, so her write failure reaches the to lookup.
	changeFixture(t, dir, `DELETE FROM message WHERE _id <> 200`, `DELETE FROM reaction`)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_group_member BEFORE INSERT ON participants WHEN NEW.phone_number = '+15555550101' BEGIN SELECT RAISE(ABORT, 'participant failure'); END`)
	require.NoError(err)

	summary := runImport(t, st, dir, storage)
	assert.Positive(summary.Errors)
	assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM messages`), "leave the message unarchived until its recipients can be written")

	_, err = st.DB().Exec(`DROP TRIGGER fail_group_member`)
	require.NoError(err)
	assert.Zero(runImport(t, st, dir, storage).Errors)
	assert.ElementsMatch([]string{signalfixture.OwnPhone, signalfixture.AlicePhone}, groupRecipientPhones(t, st))
}

func TestRerunAfterGroupRosterReadFailureRestoresRecipients(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir, storage := testutil.NewTestStore(t), t.TempDir(), t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `DELETE FROM message WHERE _id <> 200`, `DELETE FROM reaction`)
	opts := DefaultOptions()
	opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, storage
	summary, err := NewImporter(st, onStartProgress{onStart: func() {
		// Make the roster unavailable after schema detection.
		changeFixture(t, dir, `ALTER TABLE group_membership RENAME TO unreadable_membership`)
	}}).Import(t.Context(), path, opts)
	require.NoError(err)

	assert.Equal(int64(1), summary.Errors)
	assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM messages`), "leave the chat's messages unarchived until its roster can be read")

	changeFixture(t, dir, `ALTER TABLE unreadable_membership RENAME TO group_membership`)
	summary = runImport(t, st, dir, storage)
	assert.Zero(summary.Errors)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.ElementsMatch([]string{signalfixture.OwnPhone, signalfixture.AlicePhone}, groupRecipientPhones(t, st))
}

func TestRerunKeepsDocumentOccurrences(t *testing.T) {
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
	summary := runImport(t, st, dir, storage)
	assert.Zero(summary.ReactionsAdded)
	assert.Zero(summary.Errors)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE id = ?`, attachmentID))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM document_occurrences WHERE attachment_id = ?`, attachmentID))
}

func TestMigratedIncomingOnly(t *testing.T) {
	// V185 copied the incoming author into both address columns. Counting
	// those rows as owner evidence would reject this backup's own number.
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.V185, 0)
	changeFixture(t, dir, `DELETE FROM message WHERE (type & 31) IN (21, 22, 23, 24)`, `UPDATE message SET to_recipient_id = from_recipient_id WHERE thread_id IN (10, 12)`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(5), summary.MessagesAdded)
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
	// Alice sends the group message; Bob is another group member.
	changeFixture(t, dir, `UPDATE message SET from_recipient_id = 2 WHERE _id = 200`)
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
	// Alice resolves normally; only the group message's reactor conflicts.
	changeFixture(t, dir, `DELETE FROM group_membership WHERE recipient_id = 3`, `UPDATE recipient SET e164 = '+15555550102' WHERE _id = 3`, `UPDATE message SET body = 'Changed text' WHERE _id = 200`, `INSERT INTO reaction (message_id, author_id, emoji, date_sent, date_received) VALUES (200, 3, 'x', 1700000300100, 1700000300101)`)
	summary := runImport(t, st, dir, storage)
	require.Positive(summary.Errors)
	assert.Equal(before, importedMessages(t, st))
	assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Changed'`))
}
