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
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT content_hash, storage_path FROM attachments`).Scan(&hash, &storagePath))
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
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id, storage_path FROM attachments`).Scan(&attachmentID, &storagePath))
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

func TestCheckpointWriteFailureFailsImport(t *testing.T) {
	for _, stage := range []string{"batch", "final", "stats"} {
		t.Run(stage, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			if stage == "final" {
				changeFixture(t, dir, `DELETE FROM reaction`, `DELETE FROM message`)
			}
			wantError := "checkpoint failure"
			trigger := `CREATE TRIGGER fail_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs WHEN NEW.messages_processed <= 1 BEGIN SELECT RAISE(ABORT, 'checkpoint failure'); END`
			if stage == "stats" {
				wantError = "stats failure"
				trigger = `CREATE TRIGGER fail_stats BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(ABORT, 'stats failure'); END`
			}
			_, err := st.DB().ExecContext(t.Context(), trigger)
			require.NoError(err)
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, t.TempDir()
			opts.BatchSize = 1

			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.ErrorContains(err, wantError)
			if stage == "batch" {
				assert.Equal(int64(1), summary.MessagesProcessed)
			}
			var status, errorMessage string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status, error_message FROM sync_runs`).Scan(&status, &errorMessage))
			assert.Equal("failed", status)
			assert.Contains(errorMessage, wantError)
		})
	}
}

func TestFailedWriteCounts(t *testing.T) {
	assert := assert.New(t)
	st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_carol BEFORE INSERT ON messages WHEN NEW.snippet = 'Carol here' BEGIN SELECT RAISE(ABORT, 'message failure'); END`)
	require.NoError(t, err)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(int64(1), summary.Errors)
	assert.Equal(int64(1), summary.AttachmentsFound)
	assert.Zero(summary.AttachmentsMissing)
	assert.Equal(int64(1), summary.AttachmentsStored)
}

func TestRerunKeepsDocumentOccurrences(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE attachment SET content_type = 'application/pdf', file_name = 'example.pdf' WHERE _id = 500`)
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	var attachmentID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM attachments`).Scan(&attachmentID))
	_, eligible, err := st.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
	require.NoError(err)
	require.True(eligible)
	summary := runImport(t, st, dir, storage)
	assert.Zero(summary.Errors)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE id = ?`, attachmentID))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM document_occurrences WHERE attachment_id = ?`, attachmentID))
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

func TestRerunPreservesGroupRecipients(t *testing.T) {
	for _, stage := range []string{"participant write", "roster read", "changed membership"} {
		t.Run(stage, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir, storage := testutil.NewSQLiteTestStore(t), t.TempDir(), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `DELETE FROM message WHERE _id <> 200`, `DELETE FROM reaction`)
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, storage
			switch stage {
			case "participant write":
				_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_group_member BEFORE INSERT ON participants WHEN NEW.phone_number = '+15555550101' BEGIN SELECT RAISE(ABORT, 'participant failure'); END`)
				require.NoError(err)
				summary := runImport(t, st, dir, storage)
				assert.Positive(summary.Errors)
				assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM messages`), "leave the message unarchived until its recipients can be written")
				_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_group_member`)
				require.NoError(err)
			case "roster read":
				summary, err := NewImporter(st, onStartProgress{onStart: func() {
					changeFixture(t, dir, `ALTER TABLE group_membership RENAME TO unreadable_membership`)
				}}).Import(t.Context(), path, opts)
				require.NoError(err)
				assert.Equal(int64(1), summary.Errors)
				assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM messages`), "leave the chat's messages unarchived until its roster can be read")
				changeFixture(t, dir, `ALTER TABLE unreadable_membership RENAME TO group_membership`)
			case "changed membership":
				changeFixture(t, dir,
					`INSERT INTO recipient (_id, pni) VALUES (7, '0f0f0f0f-0000-4000-8000-000000000007')`,
					`INSERT INTO group_membership (group_id, recipient_id) SELECT group_id, 7 FROM group_membership WHERE recipient_id = 2`)
				runImport(t, st, dir, storage)
				changeFixture(t, dir, `UPDATE group_membership SET recipient_id = 4 WHERE recipient_id = 2`)
			}
			summary := runImport(t, st, dir, storage)
			assert.Zero(summary.Errors)
			if stage == "roster read" {
				assert.Equal(int64(1), summary.MessagesAdded)
			}
			assert.ElementsMatch([]string{signalfixture.OwnPhone, signalfixture.AlicePhone}, groupRecipientPhones(t, st))
		})
	}
}
