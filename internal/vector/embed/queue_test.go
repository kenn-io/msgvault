//go:build sqlite_vec

package embed

import (
	"context"
	"sort"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestQueue_ClaimReleaseComplete(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 5)
	q := NewQueue(db)

	ids, token, err := q.Claim(ctx, 1, 3)
	require.NoError(err, "Claim")
	require.Len(ids, 3)
	require.NotEmpty(token, "claim token")

	// Second claim sees only 2 available.
	more, token2, err := q.Claim(ctx, 1, 10)
	require.NoError(err)
	assert.Len(more, 2)
	assert.NotEqual(token, token2, "token collision")

	require.NoError(q.Release(ctx, 1, token, ids), "Release")
	assert.Equal(3, countAvailable(t, db, 1), "available after release")

	// Now complete the second batch; pending count should drop by 2.
	require.NoError(q.Complete(ctx, 1, token2, more), "Complete")
	var total int
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&total), "total")
	assert.Equal(3, total, "pending total after complete (5 - 2)")
}

func TestQueue_Claim_EmptyBatchIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 1)
	q := NewQueue(db)
	ids, token, err := q.Claim(ctx, 1, 0)
	requirepkg.NoError(t, err, "Claim(0)")
	assertpkg.Empty(t, ids)
	assertpkg.Empty(t, token)
}

func TestQueue_Claim_NoAvailableReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 0)
	q := NewQueue(db)
	ids, token, err := q.Claim(ctx, 1, 10)
	requirepkg.NoError(t, err, "Claim")
	assertpkg.Empty(t, ids)
	assertpkg.Empty(t, token)
}

func TestQueue_Complete_WrongTokenNoop(t *testing.T) {
	require := requirepkg.New(t)
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 2)
	q := NewQueue(db)
	ids, _, err := q.Claim(ctx, 1, 2)
	require.NoError(err)
	// Wrong token — rows should remain.
	require.NoError(q.Complete(ctx, 1, "deadbeef", ids), "Complete with wrong token")
	var n int
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&n))
	assertpkg.Equal(t, 2, n, "Complete should not delete on token mismatch")
}

func TestQueue_Release_WrongTokenNoop(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 2)
	q := NewQueue(db)
	ids, _, err := q.Claim(ctx, 1, 2)
	requirepkg.NoError(t, err)
	requirepkg.NoError(t, q.Release(ctx, 1, "deadbeef", ids), "Release with wrong token")
	assertpkg.Equal(t, 0, countAvailable(t, db, 1), "still claimed")
}

func TestQueue_ReclaimStale(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 2)
	q := NewQueue(db)
	_, _, err := q.Claim(ctx, 1, 2)
	require.NoError(err)
	// Back-date the claim past the threshold.
	_, err = db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = ? WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix())
	require.NoError(err)
	n, err := q.ReclaimStale(ctx, 10*time.Minute)
	require.NoError(err)
	assert.Equal(2, n, "reclaimed")
	assert.Equal(2, countAvailable(t, db, 1), "available after reclaim")
}

func TestQueue_Complete_EmptyIDsIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 1)
	q := NewQueue(db)
	assertpkg.NoError(t, q.Complete(ctx, 1, "token", nil), "Complete(nil)")
}

// TestQueue_Claim_ReturnsIDsAscending verifies that Claim's returned
// slice is sorted ascending regardless of the order SQLite's
// UPDATE...RETURNING clause produces rows. Callers (the Worker) pair
// ids with fetched message rows by position, so a non-deterministic
// order would cause silent vector↔message mixups.
func TestQueue_Claim_ReturnsIDsAscending(t *testing.T) {
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 10)
	q := NewQueue(db)

	ids, _, err := q.Claim(ctx, 1, 10)
	requirepkg.NoError(t, err, "Claim")
	requirepkg.Len(t, ids, 10)
	assertpkg.True(t, sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }),
		"ids not ascending: %v", ids)
}

// TestQueue_Complete_AfterReclaim_PreservesNewClaim simulates the
// stale-worker-completing-late race: worker A claims rows, stalls
// long enough for ReclaimStale to clear the claim, worker B
// re-claims the same rows, then worker A finally finishes and calls
// Complete with its old token. The token check must prevent A from
// deleting B's row.
func TestQueue_Complete_AfterReclaim_PreservesNewClaim(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	ctx := context.Background()
	db := openVectorsDBWithPending(t, 2)
	q := NewQueue(db)

	idsA, tokenA, err := q.Claim(ctx, 1, 2)
	require.NoError(err, "Claim A")
	require.Len(idsA, 2, "Claim A ids")

	// Back-date A's claim past the threshold, then reclaim.
	_, err = db.ExecContext(ctx,
		`UPDATE pending_embeddings SET claimed_at = ? WHERE generation_id = 1`,
		time.Now().Add(-20*time.Minute).Unix())
	require.NoError(err)
	n, err := q.ReclaimStale(ctx, 10*time.Minute)
	require.NoError(err)
	require.Equal(2, n, "ReclaimStale n")

	idsB, tokenB, err := q.Claim(ctx, 1, 2)
	require.NoError(err, "Claim B")
	require.Len(idsB, 2)
	require.NotEqual(tokenA, tokenB)

	// Stale worker A finishes and calls Complete with its dead token.
	// The token check must keep B's rows intact.
	require.NoError(q.Complete(ctx, 1, tokenA, idsA), "Complete(stale tokenA)")
	var remaining int
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining))
	require.Equal(2, remaining, "pending rows after stale Complete; stale token must not delete")

	// B's claim should still be intact (claim_token matches tokenB).
	var claimed int
	require.NoError(db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE claim_token = ?`, tokenB).Scan(&claimed))
	assert.Equal(2, claimed, "rows still holding B's token")

	// B can now legitimately Complete.
	require.NoError(q.Complete(ctx, 1, tokenB, idsB), "Complete(tokenB)")
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings`).Scan(&remaining))
	assert.Equal(0, remaining, "pending rows after B's Complete")
}
