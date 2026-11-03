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

func TestReviewLongToShortEdit(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 102, 'text/x-signal-plain')`, `UPDATE message SET body = 'Short replacement' WHERE _id = 106`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("Original long text"), 0o600))
	runImport(t, st, dir, t.TempDir())
	assert.Equal(t, "Short replacement", importedMessages(t, st)["signal:aci:"+signalfixture.AliceACI+":"+signalfixture.OwnACI+":"+ms(signalfixture.TEditOld)].body)
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'replacement'`))
}

func TestReviewLongTextRemovalKeepsOneMediaOccurrence(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, display_order) VALUES (503, 200, 'text/x-signal-plain', -1)`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("Original long text"), 0o600))
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	changeFixture(t, dir, `DELETE FROM attachment WHERE _id = 503`)
	summary := runImport(t, st, dir, storage)
	assert.Zero(t, summary.AttachmentsStored)
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT attachment_count FROM messages WHERE has_attachments = TRUE`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestScratchAttachmentKeyShift(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, file_name, display_order) VALUES (503, 200, 'image/png', 'other.png', -1)`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("other photo"), 0o600))
	summary := runImport(t, st, dir, storage)
	assert.Equal(t, int64(1), summary.AttachmentsStored)
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE filename = 'other.png'`))
	assert.Equal(t, 2, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	changeFixture(t, dir, `UPDATE attachment SET display_order = 1 WHERE _id = 503`)
	summary = runImport(t, st, dir, storage)
	assert.Zero(t, summary.AttachmentsStored)
	assert.Equal(t, 2, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	// Identical files in two positions still represent two occurrences.
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, display_order) VALUES (504, 200, 'image/jpeg', -2)`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Attachment_504_-1.bin"), []byte("photo bytes"), 0o600))
	summary = runImport(t, st, dir, storage)
	assert.Equal(t, int64(1), summary.AttachmentsStored)
	assert.Equal(t, 3, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	assert.Equal(t, 2, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE content_hash = '2a6927613ddc903379f3e02145f22074ccf048c7753cfa1ed8ee26c746682997'`))
	summary = runImport(t, st, dir, storage)
	assert.Zero(t, summary.AttachmentsStored)
	assert.Equal(t, 3, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
}

func TestReviewReactorConflictLeavesSnapshot(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	before := importedMessages(t, st)
	// A new phone for an ACI-only reactor conflicts with two curated people.
	aciPID, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.BobACI)
	require.NoError(t, err)
	_, _, err = st.CreatePersonFromParticipant(aciPID)
	require.NoError(t, err)
	phonePID, err := st.EnsureParticipantByPhone("+15555550102", "Other Example", SourceType)
	require.NoError(t, err)
	_, _, err = st.CreatePersonFromParticipant(phonePID)
	require.NoError(t, err)
	// Carol's sender resolves normally; only her reactor has a conflict.
	changeFixture(t, dir, `UPDATE recipient SET e164 = '+15555550102' WHERE _id = 3`, `UPDATE message SET body = 'Changed text' WHERE _id = 300`, `INSERT INTO reaction (message_id, author_id, emoji, date_sent, date_received) VALUES (300, 3, 'x', 1700000300100, 1700000300101)`)
	summary := runImport(t, st, dir, storage)
	require.Positive(t, summary.Errors)
	assert.Equal(t, before, importedMessages(t, st))
	assert.Equal(t, 0, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Changed'`))
}

func TestReviewUnresolvedRepliesCountOnlyThisRun(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET quote_id = 1700000999999, quote_author = 2 WHERE _id = 300`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(1), summary.RepliesUnresolved)
	changeFixture(t, dir, `DELETE FROM message WHERE _id = 300`)
	summary = runImport(t, st, dir, t.TempDir())
	assert.Zero(t, summary.RepliesUnresolved)
}
