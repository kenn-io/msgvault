package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestDeleteDedupedBatch_DeletesHiddenRows verifies that DeleteDedupedBatch removes only the
// rows associated with the given batch ID and that ON DELETE CASCADE removes
// child rows (message_labels).
func TestDeleteDedupedBatch_DeletesHiddenRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require.NoError(t, err, "DeleteDedupedBatch unknown batch")
	assert.Equal(t, int64(0), deleted, "DeleteDedupedBatch deleted")
}

func TestDeleteDedupedBatchesContext_CancellationRollsBackEveryBatch(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	f.Store.DB().SetMaxOpenConns(1)

	idKeepA := newRFC822Message(t, f, "keep-a", "rfc822-cancel-a")
	idDropA := newRFC822Message(t, f, "drop-a", "rfc822-cancel-a")
	_, err := f.Store.MergeDuplicates(idKeepA, []int64{idDropA}, "batch-alpha")
	require.NoError(err, "MergeDuplicates alpha")

	idKeepB := newRFC822Message(t, f, "keep-b", "rfc822-cancel-b")
	idDropB := newRFC822Message(t, f, "drop-b", "rfc822-cancel-b")
	_, err = f.Store.MergeDuplicates(idKeepB, []int64{idDropB}, "batch-beta")
	require.NoError(err, "MergeDuplicates beta")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	secondBatchStarted := make(chan struct{})
	conn, err := f.Store.DB().Conn(context.Background())
	require.NoError(err, "get SQLite connection")
	err = conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok, "driver connection is SQLite")
		return sqliteConn.RegisterFunc("wait_for_dedup_cancel", func() int {
			close(secondBatchStarted)
			<-ctx.Done()
			return 0
		}, true)
	})
	require.NoError(err, "register cancellation function")
	require.NoError(conn.Close(), "return SQLite connection to pool")

	_, err = f.Store.DB().Exec(`
		CREATE TRIGGER wait_before_second_dedup_batch
		BEFORE DELETE ON messages
		WHEN OLD.delete_batch_id = 'batch-beta'
		BEGIN
			SELECT wait_for_dedup_cancel();
		END
	`)
	require.NoError(err, "create cancellation trigger")

	done := make(chan error, 1)
	go func() {
		_, err := f.Store.DeleteDedupedBatchesContext(
			ctx,
			[]string{"batch-alpha", "batch-beta"},
		)
		done <- err
	}()

	select {
	case <-secondBatchStarted:
	case <-time.After(time.Second):
		require.FailNow("second batch deletion did not start")
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(time.Second):
		require.FailNow("batch deletion did not stop after cancellation")
	}
	require.ErrorIs(err, context.Canceled)

	for _, batchID := range []string{"batch-alpha", "batch-beta"} {
		var count int64
		err = f.Store.DB().QueryRow(
			"SELECT COUNT(*) FROM messages WHERE delete_batch_id = ?",
			batchID,
		).Scan(&count)
		require.NoError(err, "count %s after cancellation", batchID)
		assert.Equal(t, int64(1), count, "%s should be restored by transaction rollback", batchID)
	}
}

// TestDeleteAllDeduped_MultiplesBatches verifies that DeleteAllDeduped removes
// rows from all batches and reports the correct counts.
func TestDeleteAllDeduped_MultipleBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
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
	require.NoError(t, err, "DeleteAllDeduped empty")
	assert.Equal(t, int64(0), deleted, "deleted")
	assert.Equal(t, int64(0), batches, "distinctBatches")
}

// TestDeleteDedupedBatch_ThenUndoNoOps verifies that calling UndoDedup after DeleteDedupedBatch
// returns 0 (the rows no longer exist) without error.
func TestDeleteDedupedBatch_ThenUndoNoOps(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	idKeep := newRFC822Message(t, f, "keep", "rfc822-undo-noop")
	idDrop := newRFC822Message(t, f, "drop", "rfc822-undo-noop")

	_, err := f.Store.MergeDuplicates(idKeep, []int64{idDrop}, "batch-noop")
	require.NoError(err, "MergeDuplicates")

	_, err = f.Store.DeleteDedupedBatch("batch-noop")
	require.NoError(err, "DeleteDedupedBatch")

	restored, err := f.Store.UndoDedup("batch-noop")
	require.NoError(err, "UndoDedup after delete")
	assert.Equal(t, int64(0), restored, "UndoDedup after delete restored")
}
