package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// PostgreSQL identity sequences order allocations, not commits. A lower
// uncommitted event must fence the consumer even after a higher event commits.
func TestFilenameDiscoveryPostgresLateCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("requires two physical PostgreSQL writers")
	}
	message := f.CreateMessage("filename-late-commit")
	first := createJournalAttachment(t, f, message, "part:1", "a")
	second := createJournalAttachment(t, f, message, "part:2", "b")
	before := filenameReady(t, f.Store)
	a, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = a.Rollback() })
	b, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = b.Rollback() })
	var pidA, pidB int
	require.NoError(a.QueryRow(`SELECT pg_backend_pid()`).Scan(&pidA))
	require.NoError(b.QueryRow(`SELECT pg_backend_pid()`).Scan(&pidB))
	require.NotEqual(pidA, pidB, "the writers must be separate physical connections")
	var lower, higher int64
	_, err = a.Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "lower.pdf", first)
	require.NoError(err)
	require.NoError(a.QueryRow(`SELECT max(sequence) FROM attachment_change_log`).Scan(&lower))
	_, err = b.Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "higher.pdf", second)
	require.NoError(err)
	require.NoError(b.QueryRow(`SELECT max(sequence) FROM attachment_change_log`).Scan(&higher))
	require.Less(lower, higher)
	require.NoError(b.Commit())
	// Keep A uncommitted until the consumer's own lock-timeout result arrives.
	// No wall-clock assertion makes runner throughput a correctness condition.
	type result struct {
		batch store.FilenameDiscoveryBatch
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
		finished <- result{batch, err}
	}()
	blocked := <-finished
	require.ErrorContains(blocked.err, "lock timeout")
	still, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(before, still, "failed writer fence must not change state/ack")
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, before.Generation, first))
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, before.Generation, second))
	var checkpoint int64
	require.NoError(f.Store.DB().QueryRow(`SELECT last_sequence FROM attachment_change_consumers
		WHERE consumer_key='filename-discovery/v1'`).Scan(&checkpoint))
	assert.Equal(before.LastSequence, checkpoint)
	require.NoError(a.Commit())
	batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(2, batch.Processed)
	assert.Equal("lower.pdf", filenameEntryName(t, f.Store, before.Generation, first))
	assert.Equal("higher.pdf", filenameEntryName(t, f.Store, before.Generation, second))
	assert.Equal(higher, batch.Status.LastSequence)
	var retained sql.NullInt64
	require.NoError(f.Store.DB().QueryRow(`SELECT min(sequence) FROM attachment_change_log`).Scan(&retained))
	assert.False(retained.Valid, "both events are acknowledged only after both writers commit")
}

// A superseded sync generation is an independent, observable failing fence.
// With an attachment writer held, filename maintenance must hit its archive
// writer fence first, then reach the sync fence only after that writer commits.
func TestFilenameDiscoveryWriterFenceBeforeSyncGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("requires a PostgreSQL writer fence")
	}
	message := f.CreateMessage("filename-sync-fence")
	id := createJournalAttachment(t, f, message, "part:1", "a")
	before := filenameReady(t, f.Store)
	// Committed journal work makes the pass reach its fences; an idle pass
	// returns before taking either one.
	added := createJournalAttachment(t, f, message, "part:2", "b")
	runID, err := f.Store.StartSync(f.Source.ID, "full")
	require.NoError(err)
	scoped := f.Store.ScopedToSync(f.Source.ID, runID)
	require.NoError(f.Store.CompleteSync(runID, "complete"))
	writer, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Rollback() })
	_, err = writer.Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "pending.pdf", id)
	require.NoError(err)
	_, err = scoped.FilenameDiscoveryBatchContext(t.Context())
	require.ErrorContains(err, "lock timeout")
	require.NotErrorIs(err, store.ErrSyncRunSuperseded, "archive fence must precede the known failing sync fence")
	still, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(before, still)
	require.NoError(writer.Commit())
	_, err = scoped.FilenameDiscoveryBatchContext(t.Context())
	require.ErrorIs(err, store.ErrSyncRunSuperseded, "after the archive writer releases, the sync fence is reached")
	still, err = f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(before, still, "neither failed fence may acknowledge or apply")
	batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(2, batch.Processed)
	assert.Equal("pending.pdf", filenameEntryName(t, f.Store, before.Generation, id))
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, before.Generation, added))
}
