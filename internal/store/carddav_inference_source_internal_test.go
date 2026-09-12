package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardDAVReviewSourceTransactionRetainsAllFences(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	reader, id, _ := newPersonFactProjectionStore(t)
	book := inferenceMigrationBook(t, reader)
	inferenceNote(t, reader, id, ProvenanceExtraction, "Before")
	writer, err := OpenForTest(reader.dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(writer.Close()) })
	before, err := reader.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	require.NoError(err)
	require.NoError(reader.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
		// Establish the production read transaction, then mutate every independent
		// fence through a second connection before reloading the source components.
		first, err := reader.loadCardDAVPublicationReviewSourceTx(t.Context(), tx, id)
		require.NoError(err)
		assert.Equal(before, first)
		inferenceNote(t, writer, id, ProvenanceExtraction, "After")
		_, err = writer.db.Exec(`UPDATE persons SET vcard_uid='updated-review-uid',display_name='Updated Review Person' WHERE id=?`, id)
		require.NoError(err)
		_, err = writer.db.Exec(`UPDATE carddav_accounts SET connection_generation=connection_generation+1 WHERE id=1`)
		require.NoError(err)
		_, err = writer.db.Exec(`UPDATE carddav_address_books SET sync_revision=sync_revision+1 WHERE id=?`, book.ID)
		require.NoError(err)
		inside, err := reader.loadCardDAVPublicationReviewSourceTx(t.Context(), tx, id)
		require.NoError(err)
		assert.Equal(first, inside)
		return nil
	}))
	after, err := reader.LoadCardDAVPublicationReviewSourceContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("updated-review-uid", after.Person.VCardUID)
	assert.Equal(after.Snapshot.Profile.Person, after.Person)
	assert.NotEqual(before.Snapshot.Fingerprint, after.Snapshot.Fingerprint)
	assert.Equal(before.Inference.InferenceRevision+1, after.Inference.InferenceRevision)
	assert.Equal(before.ConnectionGeneration+1, after.ConnectionGeneration)
	assert.Equal(before.Book.SyncRevision+1, after.Book.SyncRevision)
}
