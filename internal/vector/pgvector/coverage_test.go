//go:build pgvector

package pgvector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

// TestCoverageSplit_EmbeddedBlankMissing mirrors the sqlitevec coverage
// test for the PostgreSQL backend: it proves EmbeddedMessageCount counts
// only messages with a real vector row, so the display-layer blank count
// (stamped - embedded) is a true "stamped but unembeddable" detector.
//
// Skips unless MSGVAULT_TEST_DB points at a live PostgreSQL with pgvector.
// It uses the minimal main schema the pgvector tests stand up, so live /
// stamped / missing are computed here with the same predicate
// store.CoverageCounts uses (the real CoverageCounts is exercised against
// the full schema in the backend-agnostic store coverage test).
func TestCoverageSplit_EmbeddedBlankMissing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	db := openPGTestDB(t) // skips when MSGVAULT_TEST_DB is unset
	b, err := Open(ctx, Options{DB: db, Dimension: 8})
	require.NoError(err, "Open backend")
	t.Cleanup(func() { _ = b.Close() })

	// 5 live messages: 2 embedded, 2 blank, 1 missing.
	ids := []int64{1, 2, 3, 4, 5}
	for _, id := range ids {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(err, "insert message %d", id)
	}
	embedded := []int64{1, 2}
	blanks := []int64{3, 4}
	// id 5 stays missing (embed_gen NULL).

	gen, err := b.CreateGeneration(ctx, "test-model", 8, "fp")
	require.NoError(err, "CreateGeneration")

	vec := func(seed float32) []float32 {
		v := make([]float32, 8)
		v[0] = seed
		return v
	}
	require.NoError(b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: embedded[0], Vector: vec(1)},
		{MessageID: embedded[1], Vector: vec(2)},
	}), "Upsert embedded vectors")

	// Stamp embedded + blank rows DONE; blanks get no vector.
	for _, id := range append(append([]int64{}, embedded...), blanks...) {
		_, err := db.ExecContext(ctx,
			`UPDATE messages SET embed_gen = $1 WHERE id = $2`, int64(gen), id)
		require.NoErrorf(err, "stamp embed_gen for msg %d", id)
	}

	// live / stamped / missing computed with the same predicate
	// store.CoverageCounts uses (no soft-deletes here, so the live filter is
	// just deleted_at/deleted_from_source_at IS NULL — all 5 qualify).
	var live, stamped int64
	require.NoError(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		   WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL`).Scan(&live),
		"count live")
	require.NoError(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		   WHERE embed_gen = $1 AND deleted_at IS NULL AND deleted_from_source_at IS NULL`,
		int64(gen)).Scan(&stamped), "count stamped")
	missing := live - stamped

	embeddedCount, err := b.EmbeddedMessageCount(ctx, gen)
	require.NoError(err, "EmbeddedMessageCount")
	blank := max(stamped-embeddedCount, 0)

	assert.Equal(int64(5), live, "live = all 5 messages")
	assert.Equal(int64(4), stamped, "stamped = 4 (2 embedded + 2 blank)")
	assert.Equal(int64(2), embeddedCount, "embedded = 2 (distinct message_ids with a vector)")
	assert.Equal(int64(2), blank, "blank = stamped - embedded = 2")
	assert.Equal(int64(1), missing, "missing = 1 (never stamped)")

	assert.Equal(live, embeddedCount+blank+missing,
		"invariant: live == embedded + blank + missing")
}

func TestCoverageSplit_ScopedEmbeddedHoldsInvariant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	db := openPGTestDB(t) // skips when MSGVAULT_TEST_DB is unset
	b, err := Open(ctx, Options{
		DB:         db,
		Dimension:  8,
		BuildScope: vector.NewBuildScope([]string{"sms"}),
	})
	require.NoError(err, "Open backend")
	t.Cleanup(func() { _ = b.Close() })

	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, message_type) VALUES
			(1, 'email'),
			(2, 'sms')
		ON CONFLICT (id) DO UPDATE SET
			message_type = EXCLUDED.message_type,
			deleted_at = NULL,
			deleted_from_source_at = NULL,
			embed_gen = NULL`)
	require.NoError(err, "seed scoped messages")

	gen, err := b.CreateGeneration(ctx, "test-model", 8, "fp")
	require.NoError(err, "CreateGeneration")
	require.NoError(b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0, 0, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0, 1, 0, 0, 0, 0, 0, 0}},
	}), "Upsert embedded vectors")
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = $1 WHERE id IN (1, 2)`, int64(gen))
	require.NoError(err, "stamp embedded")

	var live, stamped int64
	require.NoError(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		   WHERE message_type = 'sms'
		     AND deleted_at IS NULL
		     AND deleted_from_source_at IS NULL`).Scan(&live),
		"count scoped live")
	require.NoError(db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		   WHERE message_type = 'sms'
		     AND embed_gen = $1
		     AND deleted_at IS NULL
		     AND deleted_from_source_at IS NULL`,
		int64(gen)).Scan(&stamped), "count scoped stamped")
	missing := live - stamped

	embeddedCount, err := b.EmbeddedMessageCount(ctx, gen)
	require.NoError(err, "EmbeddedMessageCount")
	blank := max(stamped-embeddedCount, 0)

	assert.Equal(int64(1), live, "only sms is in scope")
	assert.Equal(int64(1), stamped, "only scoped stamped messages count")
	assert.Equal(int64(1), embeddedCount, "out-of-scope email vector excluded")
	assert.Equal(int64(0), blank)
	assert.Equal(int64(0), missing)
	assert.Equal(live, embeddedCount+blank+missing,
		"invariant: live == embedded + blank + missing")
}

func TestFilteredCoverageRequiresLiveGenerationStampAndVector(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	db := openPGTestDB(t)
	b, err := Open(ctx, Options{DB: db, Dimension: 8})
	require.NoError(err)
	t.Cleanup(func() { _ = b.Close() })

	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, embed_gen, deleted_at, deleted_from_source_at) VALUES
			(1, NULL, NULL, NULL),
			(2, NULL, NULL, NULL),
			(3, NULL, CURRENT_TIMESTAMP, NULL),
			(4, NULL, NULL, CURRENT_TIMESTAMP),
			(5, 999, NULL, NULL),
			(6, NULL, NULL, NULL)`)
	require.NoError(err)
	gen, err := b.CreateGeneration(ctx, "test-model", 8, "fp")
	require.NoError(err)
	_, err = db.ExecContext(ctx, `UPDATE messages SET embed_gen = $1 WHERE id IN (1, 3, 4, 6)`, int64(gen))
	require.NoError(err)
	chunks := make([]vector.Chunk, 0, 5)
	for i, id := range []int64{1, 2, 3, 4, 5} {
		v := make([]float32, 8)
		v[i] = 1
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: v})
	}
	require.NoError(b.Upsert(ctx, gen, chunks))

	count, err := b.EmbeddedMessageCountForIDs(ctx, gen, []int64{1, 2, 3, 4, 5, 6})
	require.NoError(err)
	assert.Equal(int64(1), count)

	_, err = b.EmbeddedMessageCountForIDs(ctx, gen, make([]int64, vector.FilteredCoverageBatchSize+1))
	assert.ErrorIs(err, vector.ErrCoverageBatchTooLarge)
}
