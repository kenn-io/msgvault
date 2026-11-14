//go:build pgvector

package pgvector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTracedBackendForTest stands up a per-test schema (via the primary handle),
// then opens a Backend over a SECOND, pgx-traced handle pointed at the same
// schema, so every statement the backend issues — including the tx-internal
// SET LOCAL / DELETEs in ActivateGeneration and RetireGeneration — is captured
// by the returned tracer. The primary db is returned for direct seeding/probing.
func newTracedBackendForTest(t *testing.T) (*Backend, *sqlTracer, context.Context) {
	t.Helper()
	db := openPGTestDB(t)
	schema := currentSchema(t, db)
	tracedDB, tracer := openTracedPGTestDB(t, schema)

	ctx := t.Context()
	b, err := Open(ctx, Options{DB: tracedDB, Dimension: 768})
	require.NoError(t, err, "Open backend over traced handle")
	t.Cleanup(func() { _ = b.Close() })
	return b, tracer, ctx
}

// TestRetireGeneration_DisablesStatementTimeout is the C1 regression: the
// corpus-size DELETEs in RetireGeneration must run inside a tx that has
// disabled the pool-wide statement_timeout=30s, otherwise a large retire is
// cancelled at 30s (SQLSTATE 57014) and rolls back. We assert both the behavior
// (retire succeeds and the embeddings are gone) AND, via the driver-level
// tracer, that the hatch `SET LOCAL statement_timeout = 0` is issued in the
// retire tx. A nonzero session timeout verifies that the override applies to
// the configured pool without timing out the SET LOCAL statement itself.
func TestRetireGeneration_DisablesStatementTimeout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, tracer, ctx := newTracedBackendForTest(t)

	gen := seedAndEmbed(t, b, b.db, map[int64][]float32{
		1: unitVec(768, 0),
		2: unitVec(768, 1),
		3: unitVec(768, 2),
	})
	require.Equal(3, countEmbeddingRows(t, b, gen), "precondition: generation has embeddings")

	// Use the normal pool timeout so the override itself can run on a busy
	// server. One connection keeps SET and RESET on the same session; the
	// tracer below requires the transaction to disable that timeout.
	b.db.SetMaxOpenConns(1)
	_, err := b.db.ExecContext(ctx, "SET statement_timeout = '30s'")
	require.NoError(
		err, "set session statement_timeout")

	tracer.reset()
	require.NoError(
		b.RetireGeneration(ctx, gen, true),
		"RetireGeneration must succeed with the configured session statement_timeout")

	_, err = b.db.ExecContext(ctx, "RESET statement_timeout")
	require.NoError(err, "restore statement_timeout before verification")

	// Later journal cleanup also disables the timeout; require the retire's override after BEGIN.
	statements := tracer.snapshot()
	require.GreaterOrEqual(len(statements), 2)
	assert.Equal("SET LOCAL statement_timeout = 0", statements[1],
		"retire tx must disable the pool-wide statement_timeout before changing data")
	assert.Equal(0, countEmbeddingRows(t, b, gen),
		"retired generation's embeddings must be deleted")
}

// TestActivateGeneration_DisablesStatementTimeout is the C1 regression for the
// auto-retire path inside ActivateGeneration: activating a new generation while
// a previous one is active DELETEs the demoted generation's (corpus-size)
// embeddings + pending rows in the same tx. That tx must disable the pool-wide
// statement_timeout, or a large auto-retire is cancelled at 30s and the
// activation rolls back. We seed two generations, activate the first, then
// activate the second (forcing the auto-retire of the first) under a nonzero
// session statement_timeout, and assert the activation succeeds, the first
// generation's embeddings are gone, and the hatch was issued.
func TestActivateGeneration_DisablesStatementTimeout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	b, tracer, ctx := newTracedBackendForTest(t)

	gen1 := seedAndEmbed(t, b, b.db, map[int64][]float32{
		1: unitVec(768, 0),
		2: unitVec(768, 1),
	})
	require.NoError(
		b.ActivateGeneration(ctx, gen1, true), "activate gen1")

	require.Equal(2, countEmbeddingRows(t, b, gen1), "precondition: gen1 has embeddings")

	gen2 := seedAndEmbed(t, b, b.db, map[int64][]float32{
		1: unitVec(768, 2),
		2: unitVec(768, 3),
	})

	// Match the pool timeout and require its transaction-local override in
	// the trace, without putting a 1ms deadline on that override statement.
	b.db.SetMaxOpenConns(1)
	_, err := b.db.ExecContext(ctx, "SET statement_timeout = '30s'")
	require.NoError(
		err, "set session statement_timeout")

	tracer.reset()
	require.NoError(
		b.ActivateGeneration(ctx, gen2, true),
		"ActivateGeneration must succeed with the configured session statement_timeout")

	_, err = b.db.ExecContext(ctx, "RESET statement_timeout")
	require.NoError(err, "restore statement_timeout before verification")

	statements := tracer.snapshot()
	require.GreaterOrEqual(len(statements), 2)
	assert.Equal("SET LOCAL statement_timeout = 0", statements[1],
		"activate tx must disable the pool-wide statement_timeout before changing data")
	assert.Equal(0, countEmbeddingRows(t, b, gen1),
		"auto-retired gen1's embeddings must be deleted by the activation")
	require. // Sanity: gen2 is now the active generation with its embeddings intact.
			Equal(2, countEmbeddingRows(t, b, gen2), "gen2 embeddings must survive activation")
}
