package store_test

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestDeleteDedupedBatch_DeletesHiddenRows verifies that DeleteDedupedBatch removes only the
// rows associated with the given batch ID and that ON DELETE CASCADE removes
// child rows (message_labels).
func TestDeleteDedupedBatch_DeletesHiddenRows(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	idKeep := newRFC822Message(t, f, "keep", "rfc822-delete-a")
	idDrop := newRFC822Message(t, f, "drop", "rfc822-delete-a")

	labels := f.EnsureLabels(
		map[string]string{"INBOX": "Inbox", "SENT": "Sent"}, "system",
	)
	require.NoError(f.Store.LinkMessageLabel(idDrop, labels["INBOX"]), "link INBOX")
	require.NoError(f.Store.LinkMessageLabel(idDrop, labels["SENT"]), "link SENT")

	_, err := f.Store.MergeDuplicates(idKeep, []int64{idDrop}, "batch-delete")
	require.NoError(err, "MergeDuplicates")

	// idDrop should be hidden before delete.
	assertDedupDeleted(t, f.Store, idDrop, true)

	deleted, err := f.Store.DeleteDedupedBatch("batch-delete")
	require.NoError(err, "DeleteDedupedBatch")
	assert.Equal(int64(1), deleted, "DeleteDedupedBatch deleted")

	// Row should be gone.
	var count int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id = ?"), idDrop,
	).Scan(&count)
	require.NoError(err, "query messages after delete")
	assert.Equal(0, count, "message %d still present after delete", idDrop)

	// Child message_labels rows should cascade-delete.
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM message_labels WHERE message_id = ?"), idDrop,
	).Scan(&count)
	require.NoError(err, "query message_labels after delete")
	assert.Equal(0, count, "message_labels for %d still present after delete", idDrop)

	// Survivor should be untouched.
	assertDedupDeleted(t, f.Store, idKeep, false)
}

// TestDeleteDedupedBatch_UnknownBatch verifies that DeleteDedupedBatch with a non-existent
// batch ID returns 0 without error.
func TestDeleteDedupedBatch_UnknownBatch(t *testing.T) {
	f := storetest.New(t)
	_ = newRFC822Message(t, f, "msg-a", "rfc822-only")

	deleted, err := f.Store.DeleteDedupedBatch("no-such-batch")
	requirepkg.NoError(t, err, "DeleteDedupedBatch unknown batch")
	assertpkg.Equal(t, int64(0), deleted, "DeleteDedupedBatch deleted")
}

// TestDeleteAllDeduped_MultiplesBatches verifies that DeleteAllDeduped removes
// rows from all batches and reports the correct counts.
func TestDeleteAllDeduped_MultipleBatches(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// batch-alpha hides one message
	idKeepA := newRFC822Message(t, f, "keep-a", "rfc822-multi-a")
	idDropA := newRFC822Message(t, f, "drop-a", "rfc822-multi-a")
	_, err := f.Store.MergeDuplicates(idKeepA, []int64{idDropA}, "batch-alpha")
	require.NoError(err, "MergeDuplicates alpha")

	// batch-beta hides one message
	idKeepB := newRFC822Message(t, f, "keep-b", "rfc822-multi-b")
	idDropB := newRFC822Message(t, f, "drop-b", "rfc822-multi-b")
	_, err = f.Store.MergeDuplicates(idKeepB, []int64{idDropB}, "batch-beta")
	require.NoError(err, "MergeDuplicates beta")

	deleted, batches, err := f.Store.DeleteAllDeduped()
	require.NoError(err, "DeleteAllDeduped")
	assert.Equal(int64(2), deleted, "DeleteAllDeduped deleted")
	assert.Equal(int64(2), batches, "DeleteAllDeduped distinctBatches")

	// All four messages should still exist (survivors untouched).
	var count int
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	require.NoError(err, "count messages after DeleteAllDeduped")
	assert.Equal(2, count, "messages count (survivors only)")
}

// TestDeleteAllDeduped_PreservesBatchlessSoftDelete verifies that a row with
// deleted_at set but no delete_batch_id is *not* purged by DeleteAllDeduped.
// The contract is "permanently remove rows the dedup pipeline soft-hid",
// keyed on the positive delete_batch_id marker. A future feature that writes
// deleted_at for any other reason (trash view, per-message hide) must not
// have its rows silently destroyed by the dedup hard-delete rung.
func TestDeleteAllDeduped_PreservesBatchlessSoftDelete(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// One real dedup batch — should be purged.
	idKeep := newRFC822Message(t, f, "keep", "rfc822-batchless")
	idDrop := newRFC822Message(t, f, "drop", "rfc822-batchless")
	_, err := f.Store.MergeDuplicates(idKeep, []int64{idDrop}, "batch-real")
	require.NoError(err, "MergeDuplicates")

	// One row soft-hidden without a batch ID — simulates a future
	// non-dedup soft-delete writer. Should survive DeleteAllDeduped.
	idBatchless := newRFC822Message(t, f, "batchless", "rfc822-other")
	_, err = f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"),
		idBatchless,
	)
	require.NoError(err, "set batchless deleted_at")

	deleted, batches, err := f.Store.DeleteAllDeduped()
	require.NoError(err, "DeleteAllDeduped")
	assert.Equal(int64(1), deleted, "DeleteAllDeduped deleted (only the batched row)")
	assert.Equal(int64(1), batches, "DeleteAllDeduped distinctBatches")

	// The batchless row must still exist after the purge.
	var count int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id = ?"), idBatchless,
	).Scan(&count)
	require.NoError(err, "query batchless row after delete")
	assert.Equal(1, count, "batchless soft-deleted row %d was purged; DeleteAllDeduped must only touch dedup-batched rows", idBatchless)
}

// TestDeleteAllDeduped_Empty verifies that DeleteAllDeduped with no hidden rows
// returns 0/0 without error.
func TestDeleteAllDeduped_Empty(t *testing.T) {
	f := storetest.New(t)
	_ = newRFC822Message(t, f, "visible", "rfc822-vis")

	deleted, batches, err := f.Store.DeleteAllDeduped()
	requirepkg.NoError(t, err, "DeleteAllDeduped empty")
	assertpkg.Equal(t, int64(0), deleted, "deleted")
	assertpkg.Equal(t, int64(0), batches, "distinctBatches")
}

// TestDeleteDedupedBatch_ThenUndoNoOps verifies that calling UndoDedup after DeleteDedupedBatch
// returns 0 (the rows no longer exist) without error.
func TestDeleteDedupedBatch_ThenUndoNoOps(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)
	idKeep := newRFC822Message(t, f, "keep", "rfc822-undo-noop")
	idDrop := newRFC822Message(t, f, "drop", "rfc822-undo-noop")

	_, err := f.Store.MergeDuplicates(idKeep, []int64{idDrop}, "batch-noop")
	require.NoError(err, "MergeDuplicates")

	_, err = f.Store.DeleteDedupedBatch("batch-noop")
	require.NoError(err, "DeleteDedupedBatch")

	restored, err := f.Store.UndoDedup("batch-noop")
	require.NoError(err, "UndoDedup after delete")
	assertpkg.Equal(t, int64(0), restored, "UndoDedup after delete restored")
}
