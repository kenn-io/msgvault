//go:build sqlite_vec

package embed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnqueuer_NoGenerations_Noop(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{1, 2, 3}), "EnqueueMessages with no generations")
	// Should be no pending rows.
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_embeddings`).Scan(&n))
	assert.Equal(t, 0, n, "pending count")
}

func TestEnqueuer_ActiveGenerationOnly(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{10, 11}))
	assertPending(t, db, 1, 2)
}

func TestEnqueuer_ActiveAndBuilding_DualEnqueue(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	insertGenerationStatic(t, db, 2, "building")
	insertGenerationStatic(t, db, 3, "retired") // should NOT receive.
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{100}))
	assertPending(t, db, 1, 1)
	assertPending(t, db, 2, 1)
	assertPending(t, db, 3, 0)
}

func TestEnqueuer_DuplicateIDs_Ignored(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	e := NewEnqueuer(db)
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42}))
	// Second call with same ID should not error; count still 1.
	require.NoError(t, e.EnqueueMessages(ctx, []int64{42, 42}))
	assertPending(t, db, 1, 1)
}

func TestEnqueuer_EmptyIDs_Noop(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBForEnqueue(t)
	insertGenerationStatic(t, db, 1, "active")
	e := NewEnqueuer(db)
	assert.NoError(t, e.EnqueueMessages(ctx, nil), "EnqueueMessages(nil)")
	assert.NoError(t, e.EnqueueMessages(ctx, []int64{}), "EnqueueMessages([])")
	assertPending(t, db, 1, 0)
}
