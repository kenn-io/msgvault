//go:build sqlite_vec && pgvector

package cmd

import (
	"context"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// TestHydrateHybridResults_PostgresRebindsINClause is the core
// regression guard for the PG search path. hydrateHybridResults builds a
// `WHERE m.id IN (?,?,...)` query; pgx rejects bare ? placeholders with
// SQLSTATE 42601 (syntax error). The PostgreSQL search branch threads
// dialect.Rebind so the placeholders become $1,$2,...; this test proves
// (a) the rebound query runs live on pgx and returns the rows in the
// ranked order of the input hits, and (b) the un-rebound (identity)
// query fails with a syntax error — the fails-without/passes-with check.
func TestHydrateHybridResults_PostgresRebindsINClause(t *testing.T) {
	_, dsn := openServePGSchema(t)

	st, err := store.Open(dsn)
	require.NoError(t, err, "store.Open on PG DSN")
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()
	require.True(t, st.IsPostgreSQL(), "store must report PostgreSQL for a postgres:// DSN")

	ctx := context.Background()

	// Minimal scaffolding for the columns hydrateHybridResults selects.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE participants (
			id BIGINT PRIMARY KEY,
			email_address TEXT
		);
		CREATE TABLE messages (
			id BIGINT PRIMARY KEY,
			source_id BIGINT,
			sender_id BIGINT,
			subject TEXT,
			sent_at TIMESTAMPTZ
		);`)
	require.NoError(t, err, "create scaffolding tables")

	_, err = db.ExecContext(ctx, `
		INSERT INTO participants (id, email_address) VALUES
			(10, 'alice@example.com'),
			(20, 'bob@example.com'),
			(30, 'carol@example.com');`)
	require.NoError(t, err, "seed participants")

	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, sender_id, subject, sent_at) VALUES
			(1, 10, 'first subject',  TIMESTAMPTZ '2025-01-01 00:00:00+00'),
			(2, 20, 'second subject', TIMESTAMPTZ '2025-02-01 00:00:00+00'),
			(3, 30, 'third subject',  TIMESTAMPTZ '2025-03-01 00:00:00+00');`)
	require.NoError(t, err, "seed messages")

	// Hits in a deliberately non-id order so the test proves the output
	// preserves the input (RRF) ranking rather than the DB's natural order.
	hits := []vector.FusedHit{
		{MessageID: 3, RRFScore: 0.9, BM25Score: 0.5, VectorScore: 0.4},
		{MessageID: 1, RRFScore: 0.8, BM25Score: 0.3, VectorScore: 0.5, SubjectBoosted: true},
		{MessageID: 2, RRFScore: 0.7, BM25Score: 0.2, VectorScore: 0.6},
	}

	rebind := (&store.PostgreSQLDialect{}).Rebind

	// passes-with: the rebound query runs on pgx and returns ranked rows.
	results, err := hydrateHybridResults(ctx, db, rebind, hits)
	require.NoError(t, err, "hydrateHybridResults must succeed with the dialect rebind on pgx")
	require.Len(t, results, 3, "all three seeded messages hydrate")

	// Order must match the input hit ranking (3, 1, 2).
	assert.Equal(t, int64(3), results[0].MessageID, "first result preserves hit[0]")
	assert.Equal(t, int64(1), results[1].MessageID, "second result preserves hit[1]")
	assert.Equal(t, int64(2), results[2].MessageID, "third result preserves hit[2]")

	// Hydrated fields come from the joined rows.
	assert.Equal(t, "third subject", results[0].Subject)
	assert.Equal(t, "carol@example.com", results[0].FromEmail)
	assert.Equal(t, "first subject", results[1].Subject)
	assert.Equal(t, "alice@example.com", results[1].FromEmail)
	assert.True(t, results[1].SubjectBoosted, "hit-level SubjectBoosted carried through")
	assert.Equal(t, "bob@example.com", results[2].FromEmail)

	// Scores from the hits flow through unchanged.
	assert.InDelta(t, 0.9, results[0].RRFScore, 1e-9)
	assert.InDelta(t, 0.6, results[2].VectorScore, 1e-9)

	// fails-without: identity rebind leaves bare ? placeholders, which pgx
	// rejects with a syntax error (SQLSTATE 42601). This proves the rebind
	// is load-bearing, not incidental.
	identity := func(s string) string { return s }
	_, err = hydrateHybridResults(ctx, db, identity, hits)
	require.Error(t, err, "un-rebound ? placeholders must fail on pgx")
	assert.Contains(t, err.Error(), "42601",
		"expected pgx syntax error (SQLSTATE 42601) for bare ? placeholders; got %v", err)
}
