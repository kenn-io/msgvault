//go:build pgvector

package pgvector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// pendingIDs returns the message_ids queued for gen, sorted, so tests
// can assert exactly which rows the catch-up recovered.
func pendingIDs(ctx context.Context, t *testing.T, b *Backend, gen vector.GenerationID) []int64 {
	t.Helper()
	rows, err := b.db.QueryContext(ctx,
		`SELECT message_id FROM pending_embeddings WHERE generation_id = $1 ORDER BY message_id`,
		int64(gen))
	require.NoError(t, err, "query pending ids")
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id), "scan pending id")
		out = append(out, id)
	}
	require.NoError(t, rows.Err(), "iterate pending ids")
	return out
}

// TestBackend_CatchUpPending_RecoversMissedEnqueue reproduces the sync
// enqueue gap: a message is committed to `messages` but its
// pending_embeddings row is missing (a failed enqueue that sync did not
// retry because the next sync skipped the message as already-ingested).
// CatchUpPending must re-enqueue it against the active generation.
func TestBackend_CatchUpPending_RecoversMissedEnqueue(t *testing.T) {
	b, ctx, db := newBackendForTest(t) // seeds message id=1

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")

	// The active generation already has id=1 queued from the seed. Now a
	// later sync persists message id=2 but its enqueue failed, so it is
	// in `messages` yet absent from pending_embeddings — the exact bug.
	_, err = db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2)`)
	require.NoError(t, err, "insert later message")
	require.Equal(t, []int64{1}, pendingIDs(ctx, t, b, gen),
		"precondition: id=2 must be missing from the queue")

	caught, err := b.CatchUpPending(ctx, gen)
	require.NoError(t, err, "CatchUpPending")
	assert.Equal(t, int64(1), caught, "should re-enqueue exactly the one missed message")
	assert.Equal(t, []int64{1, 2}, pendingIDs(ctx, t, b, gen),
		"missed message must be queued after catch-up")
}

// TestBackend_CatchUpPending_Idempotent verifies the catch-up is a cheap
// no-op when nothing is missing: a second call inserts zero rows and the
// queue is unchanged.
func TestBackend_CatchUpPending_Idempotent(t *testing.T) {
	b, ctx, db := newBackendForTest(t) // seeds message id=1

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")

	_, err = db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2)`)
	require.NoError(t, err, "insert later message")

	first, err := b.CatchUpPending(ctx, gen)
	require.NoError(t, err, "first CatchUpPending")
	assert.Equal(t, int64(1), first, "first call recovers the missed message")

	second, err := b.CatchUpPending(ctx, gen)
	require.NoError(t, err, "second CatchUpPending")
	assert.Equal(t, int64(0), second, "second call is a no-op (nothing missing)")
	assert.Equal(t, []int64{1, 2}, pendingIDs(ctx, t, b, gen),
		"queue unchanged after the idempotent second call")
}

// TestBackend_CatchUpPending_SkipsDeleted ensures the catch-up honours
// the live-message predicate so soft-deleted / source-deleted rows are
// not re-enqueued.
func TestBackend_CatchUpPending_SkipsDeleted(t *testing.T) {
	b, ctx, db := newBackendForTest(t) // seeds live message id=1

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")

	// id=2 source-deleted, id=3 dedup-deleted: both must be skipped.
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, deleted_from_source_at) VALUES (2, NOW())`)
	require.NoError(t, err, "insert source-deleted message")
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, deleted_at) VALUES (3, NOW())`)
	require.NoError(t, err, "insert dedup-deleted message")

	caught, err := b.CatchUpPending(ctx, gen)
	require.NoError(t, err, "CatchUpPending")
	assert.Equal(t, int64(0), caught, "deleted messages must not be re-enqueued")
	assert.Equal(t, []int64{1}, pendingIDs(ctx, t, b, gen),
		"only the live message stays queued")
}

// TestBackend_CatchUpPending_RejectsRetired guards the invariant that
// retired generations carry no pending rows: a catch-up against a
// retired generation must refuse rather than orphan rows that no future
// run would drain.
func TestBackend_CatchUpPending_RejectsRetired(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	gen, err := b.CreateGeneration(ctx, "m", 768, "")
	require.NoError(t, err, "Create")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")
	require.NoError(t, b.RetireGeneration(ctx, gen, true), "Retire")

	_, err = db.ExecContext(ctx, `INSERT INTO messages (id) VALUES (2)`)
	require.NoError(t, err, "insert later message")

	_, err = b.CatchUpPending(ctx, gen)
	require.ErrorIs(t, err, vector.ErrUnknownGeneration,
		"catch-up on a retired generation must be rejected")
	assert.Empty(t, pendingIDs(ctx, t, b, gen),
		"retired generation must remain free of pending rows")
}

// TestBackend_CatchUpPending_UnknownGeneration surfaces
// ErrUnknownGeneration for a bogus generation id.
func TestBackend_CatchUpPending_UnknownGeneration(t *testing.T) {
	b, ctx, _ := newBackendForTest(t)
	_, err := b.CatchUpPending(ctx, vector.GenerationID(999))
	assert.ErrorIs(t, err, vector.ErrUnknownGeneration,
		"catch-up on an unknown generation must surface ErrUnknownGeneration")
}
