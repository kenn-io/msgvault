//go:build pgvector

package pgvector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// insertBuildingGeneration inserts a 'building' index_generations row with an
// explicit id (no pending rows yet) so a test can drive seedPending against a
// generation in its pre-seed state. Mirrors the enqueue test's
// insertPGGeneration helper.
func insertBuildingGeneration(t *testing.T, b *Backend, id int64, dim int) vector.GenerationID {
	t.Helper()
	_, err := b.db.ExecContext(context.Background(), `
		INSERT INTO index_generations (id, model, dimension, fingerprint, started_at, state)
		OVERRIDING SYSTEM VALUE
		VALUES ($1, 'm', $2, $3, 0, 'building')`,
		id, dim, "m:768")
	require.NoError(t, err, "insert building generation")
	return vector.GenerationID(id)
}

// TestBackend_SeedPending_RetireDuringSeed_NoOrphan drives the concurrent
// retire-during-seed interleaving that seedPending's locked re-validation
// closes. It forces the exact window the orphan-pending race opens — the seed
// tx begins, THEN a concurrent RetireGeneration commits (UPDATE state='retired'
// + DELETE pending), THEN seedPending re-reads the generation under
// FOR NO KEY UPDATE and runs its INSERT … SELECT — and asserts no pending row
// is left behind for the now-retired generation.
//
// Without the lock+recheck (the bug), seedPending's INSERT … SELECT runs after
// retire's DELETE has already cleared the generation's pending rows, so it
// inserts fresh pending rows for a retired generation that no worker will ever
// target — orphan work. With the fix the locked re-read observes
// state='retired' and skips the insert, so the post-state has zero pending rows
// for that generation.
//
// The interleave is made deterministic via afterSeedLockHook: the hook fires
// inside the seed tx after BEGIN but before the locked re-read, and runs the
// retire to completion before returning, so seedPending's re-validation always
// observes the committed retire. Mirrors the Enqueuer's
// TestEnqueuerPG_RetireDuringEnqueue_NoOrphan.
func TestBackend_SeedPending_RetireDuringSeed_NoOrphan(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	const dim = 768

	// Several live messages so that, absent the fix, the INSERT … SELECT would
	// actually insert orphan pending rows (newBackendForTest already created
	// message id=1; add more to make the orphan obvious).
	for _, id := range []int64{2, 3, 4} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}

	// A building generation in its pre-seed state (no pending rows yet).
	gen := insertBuildingGeneration(t, b, 1, dim)

	// The hook fires once, inside the seed tx, after BEGIN but before the
	// locked re-read. We retire the building generation to completion here
	// (non-force is permitted — it is not active) so seedPending's subsequent
	// FOR NO KEY UPDATE re-read observes the committed state='retired'. Reset
	// the seam so it cannot leak into sibling tests sharing this package's
	// globals.
	var retireErr error
	afterSeedLockHook = func() {
		retireErr = b.RetireGeneration(ctx, gen, false)
	}
	t.Cleanup(func() { afterSeedLockHook = nil })

	require.NoError(t, b.seedPending(ctx, gen, 0), "seedPending")
	require.NoError(t, retireErr, "RetireGeneration during seed")

	// The generation was retired before seedPending inserted its rows: the
	// locked re-validation must have skipped the insert, leaving zero orphan
	// pending rows.
	assert.Equal(t, 0, countPendingRows(t, b, gen),
		"retired-mid-seed generation must have no orphan pending rows")

	// Sanity: the generation really is retired.
	var state string
	require.NoError(t, b.db.QueryRowContext(ctx,
		`SELECT state FROM index_generations WHERE id = $1`, int64(gen)).Scan(&state))
	assert.Equal(t, string(vector.GenerationRetired), state, "generation retired")
}

// TestBackend_SeedPending_SeedsBuildingGeneration is the control: with no
// concurrent retire, seedPending populates one pending_embeddings row per live
// message for a building generation. Confirms the new lock+recheck does not
// regress the normal seed path.
func TestBackend_SeedPending_SeedsBuildingGeneration(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	const dim = 768
	for _, id := range []int64{2, 3, 4} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}

	gen := insertBuildingGeneration(t, b, 1, dim)
	require.NoError(t, b.seedPending(ctx, gen, 0), "seedPending")

	// One pending row per live message (ids 1..4).
	assert.Equal(t, 4, countPendingRows(t, b, gen),
		"seedPending must enqueue one pending row per live message")
}
