//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// sqlTracer is a pgx QueryTracer that records the SQL text of every query
// the driver executes — including statements issued INSIDE a transaction
// (SET LOCAL, schema apply, DROP INDEX). database/sql's *sql.Tx is a
// concrete type the migrateExecer wrapper can't intercept, so a driver-level
// tracer is the only reliable way to observe tx-internal statements. The B2
// assertion (SET LOCAL statement_timeout=0 is issued, DDL wrapped in the tx)
// depends on this. Test-only.
type sqlTracer struct {
	mu  sync.Mutex
	got []string
}

func (t *sqlTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.got = append(t.got, data.SQL)
	t.mu.Unlock()
	return ctx
}

func (t *sqlTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *sqlTracer) contains(sub string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.got {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (t *sqlTracer) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.got))
	copy(out, t.got)
	return out
}

// reset clears the captured SQL stream so a test can scope subsequent
// assertions to statements issued AFTER the reset point (e.g. ignore the
// seed/migrate traffic and only observe a retire/activate tx). Test-only.
func (t *sqlTracer) reset() {
	t.mu.Lock()
	t.got = t.got[:0]
	t.mu.Unlock()
}

// recordingExecer wraps a real *sql.DB and captures every SQL string that
// Migrate executes through the migrateExecer interface — the pool-level
// ExecContext (where CREATE EXTENSION runs) and the BeginTx invocation
// (recorded as a marker so the B2 test can assert the DDL is wrapped). It
// delegates to the real DB so the schema is actually applied. Statements run
// INSIDE the returned *sql.Tx are observed via the pgx tracer instead (see
// sqlTracer); the optional tracer field links the two so callers can query a
// single combined view. Test-only.
type recordingExecer struct {
	db     *sql.DB
	tracer *sqlTracer
	mu     sync.Mutex
	got    []string
}

func newRecordingExecer(db *sql.DB, tracer *sqlTracer) *recordingExecer {
	return &recordingExecer{db: db, tracer: tracer}
}

func (r *recordingExecer) record(query string) {
	r.mu.Lock()
	r.got = append(r.got, query)
	r.mu.Unlock()
}

func (r *recordingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	r.record(query)
	return r.db.ExecContext(ctx, query, args...)
}

// BeginTx records that the maintenance transaction was opened, then returns
// the real *sql.Tx. Statements run on that tx are captured by the pgx tracer.
func (r *recordingExecer) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	tx, err := r.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	r.record(beginTxMarker)
	return tx, nil
}

const beginTxMarker = "<<BEGIN TX>>"

// execerContains reports whether the execer-level captures contain sub.
func (r *recordingExecer) execerContains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.got {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (r *recordingExecer) beginTxInvoked() bool { return r.execerContains(beginTxMarker) }

// openTracedPGTestDB opens a SECOND handle to the same per-test schema as db,
// configured with a pgx QueryTracer so every executed statement (including
// tx-internal ones) is captured. It mirrors openPGTestDB's search_path so the
// `vector` type and the per-test embeddings table resolve identically. The
// returned tracer accumulates the SQL stream; the *sql.DB is closed via
// t.Cleanup.
func openTracedPGTestDB(t *testing.T, schema string) (*sql.DB, *sqlTracer) {
	t.Helper()
	url := testDBURL(t)

	cfg, err := pgx.ParseConfig(url)
	require.NoError(t, err, "parse pgx config")
	tracer := &sqlTracer{}
	cfg.Tracer = tracer
	// Resolve the per-test schema first, then public (pgvector's extension).
	cfg.RuntimeParams["search_path"] = schema + ",public"

	connStr := stdlib.RegisterConnConfig(cfg)
	db, err := sql.Open("pgx", connStr)
	require.NoError(t, err, "open traced db")
	t.Cleanup(func() {
		_ = db.Close()
		stdlib.UnregisterConnConfig(connStr)
	})
	return db, tracer
}

// currentSchema returns the connection's first search_path schema, used to
// point the traced handle at the same per-test schema as the primary handle.
func currentSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var sp string
	require.NoError(t, db.QueryRow("SELECT current_schema()").Scan(&sp), "current_schema")
	return sp
}

// indexNames returns the names of every index on the embeddings table in
// the connection's own (per-test) schema. Used by the V3 and V5 tests to
// assert which indexes Migrate did/did not create. The query resolves the
// embeddings table through the search_path (to_regclass) and scopes
// pg_index to THAT relation, so indexes belonging to sibling test schemas'
// embeddings tables never leak into the result.
func indexNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`
		SELECT ic.relname
		  FROM pg_index i
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		 WHERE i.indrelid = to_regclass('embeddings')`)
	require.NoError(t, err, "query pg_index")
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name), "scan indexname")
		out[name] = true
	}
	require.NoError(t, rows.Err(), "iterate pg_index")
	return out
}

// TestMigrate_DropsRedundantGenMsgIndex (V3) asserts that after Migrate the
// redundant idx_embeddings_gen_msg index is ABSENT (it is a pure
// leading-prefix of the PK), while the embeddings primary key and the
// still-needed idx_embeddings_msg index are PRESENT.
func TestMigrate_DropsRedundantGenMsgIndex(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx, db, 768, false), "Migrate")

	idx := indexNames(t, db)
	assert.False(t, idx["idx_embeddings_gen_msg"],
		"idx_embeddings_gen_msg must be absent (redundant PK prefix); got %v", idx)
	assert.True(t, idx["idx_embeddings_msg"],
		"idx_embeddings_msg must be present; got %v", idx)
	assert.True(t, idx["embeddings_pkey"],
		"embeddings primary key index must be present; got %v", idx)
}

// TestMigrate_DropsPreExistingGenMsgIndex (V3) seeds the legacy index by
// hand (as an old DB would have it) and asserts Migrate sheds it via the
// DROP INDEX IF EXISTS step.
func TestMigrate_DropsPreExistingGenMsgIndex(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	// First migrate to create the embeddings table, then recreate the
	// legacy index to simulate a DB provisioned before V3.
	require.NoError(t, Migrate(ctx, db, 0, false), "first Migrate")
	_, err := db.ExecContext(ctx,
		`CREATE INDEX idx_embeddings_gen_msg ON embeddings(generation_id, message_id)`)
	require.NoError(t, err, "recreate legacy index")
	require.True(t, indexNames(t, db)["idx_embeddings_gen_msg"], "legacy index should exist before re-migrate")

	// Re-running Migrate must drop it.
	require.NoError(t, Migrate(ctx, db, 0, false), "second Migrate")
	assert.False(t, indexNames(t, db)["idx_embeddings_gen_msg"],
		"re-migrate must drop the legacy idx_embeddings_gen_msg")
}

// TestMigrate_SkipExtension (V5 / finding B3) asserts that the skipExtension
// flag is HONORED — not merely that the schema objects exist (which would also
// be true for an impl that ignored the flag and ran the harmless
// `CREATE EXTENSION IF NOT EXISTS vector` no-op). It records the SQL stream
// through the migrateExecer seam + a pgx tracer and asserts:
//   - skipExtension=true  -> NO "CREATE EXTENSION" statement is issued.
//   - skipExtension=false -> a "CREATE EXTENSION" statement IS issued.
//
// It also pins finding B2: the schema apply + DROP INDEX run inside a
// transaction that issued `SET LOCAL statement_timeout = 0` (the S1
// maintenance hatch), so the DDL cannot be cancelled by the pool-wide 30s
// timeout. The CREATE EXTENSION stays OUTSIDE the tx (run on the pool).
func TestMigrate_SkipExtension(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	schema := currentSchema(t, db)

	// ---- skipExtension = true: CREATE EXTENSION must NOT appear ----
	tracedDB, tracer := openTracedPGTestDB(t, schema)
	rec := newRecordingExecer(tracedDB, tracer)
	require.NoError(t, Migrate(ctx, rec, 768, true), "Migrate(skipExtension=true)")

	assert.False(t, rec.execerContains("CREATE EXTENSION"),
		"skipExtension=true must NOT issue CREATE EXTENSION on the pool; got %v", rec.got)
	assert.False(t, tracer.contains("CREATE EXTENSION"),
		"skipExtension=true must NOT issue CREATE EXTENSION at all; got %v", tracer.snapshot())

	// B2: DDL is wrapped in a tx and the statement_timeout hatch fired.
	assert.True(t, rec.beginTxInvoked(),
		"Migrate must open a maintenance tx for the schema apply + DROP INDEX")
	assert.True(t, tracer.contains("SET LOCAL statement_timeout = 0"),
		"Migrate tx must disable the pool-wide statement_timeout (S1 hatch); got %v", tracer.snapshot())
	assertHatchedDDL(t, tracer)

	// Schema tables exist.
	for _, table := range []string{"index_generations", "embeddings", "pending_embeddings", "embed_runs"} {
		var reg sql.NullString
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT to_regclass($1)::text`, table).Scan(&reg),
			"to_regclass %s", table)
		assert.Truef(t, reg.Valid, "table %s must exist after skip-extension migrate", table)
	}
	// Indexes exist (and the redundant one is still dropped).
	idx := indexNames(t, db)
	assert.True(t, idx["idx_embeddings_msg"], "idx_embeddings_msg must exist; got %v", idx)
	assert.True(t, idx["embeddings_pkey"], "PK must exist; got %v", idx)
	assert.False(t, idx["idx_embeddings_gen_msg"], "redundant index must be absent; got %v", idx)
	// HNSW index for the eager dimension exists.
	assert.Truef(t, idx[VectorIndexName(768)], "eager HNSW index %s must exist; got %v", VectorIndexName(768), idx)

	// ---- skipExtension = false: CREATE EXTENSION MUST appear ----
	db2 := openPGTestDB(t)
	schema2 := currentSchema(t, db2)
	tracedDB2, tracer2 := openTracedPGTestDB(t, schema2)
	rec2 := newRecordingExecer(tracedDB2, tracer2)
	require.NoError(t, Migrate(ctx, rec2, 0, false), "Migrate(skipExtension=false)")

	assert.True(t, rec2.execerContains("CREATE EXTENSION"),
		"skipExtension=false must issue CREATE EXTENSION on the pool; got %v", rec2.got)
	// CREATE EXTENSION runs on the pool (execer), BEFORE the tx — it must NOT
	// be inside the tx stream that carries the SET LOCAL hatch.
	assertCreateExtensionOutsideTx(t, tracer2)
}

// assertHatchedDDL asserts the schema apply and DROP INDEX appear in the
// traced stream AFTER the SET LOCAL statement_timeout=0 statement, i.e. they
// run inside the hatched maintenance transaction.
func assertHatchedDDL(t *testing.T, tracer *sqlTracer) {
	t.Helper()
	stream := tracer.snapshot()
	setLocalIdx, schemaIdx, dropIdx := -1, -1, -1
	for i, s := range stream {
		switch {
		case strings.Contains(s, "SET LOCAL statement_timeout = 0"):
			if setLocalIdx == -1 {
				setLocalIdx = i
			}
		case strings.Contains(s, "CREATE TABLE IF NOT EXISTS index_generations"):
			schemaIdx = i
		case strings.Contains(s, "DROP INDEX IF EXISTS idx_embeddings_gen_msg"):
			dropIdx = i
		}
	}
	require.NotEqual(t, -1, setLocalIdx, "SET LOCAL statement_timeout=0 must be issued; got %v", stream)
	require.NotEqual(t, -1, schemaIdx, "schema apply must be issued; got %v", stream)
	require.NotEqual(t, -1, dropIdx, "DROP INDEX must be issued; got %v", stream)
	assert.Greater(t, schemaIdx, setLocalIdx, "schema apply must run after SET LOCAL (inside the hatched tx)")
	assert.Greater(t, dropIdx, setLocalIdx, "DROP INDEX must run after SET LOCAL (inside the hatched tx)")
}

// assertCreateExtensionOutsideTx asserts CREATE EXTENSION is issued BEFORE the
// SET LOCAL statement_timeout=0 that opens the maintenance tx, i.e. it runs on
// the pool outside the tx (finding B2 keeps the fast, superuser-gated
// CREATE EXTENSION off the hatched tx).
func assertCreateExtensionOutsideTx(t *testing.T, tracer *sqlTracer) {
	t.Helper()
	stream := tracer.snapshot()
	createIdx, setLocalIdx := -1, -1
	for i, s := range stream {
		switch {
		case strings.Contains(s, "CREATE EXTENSION"):
			createIdx = i
		case strings.Contains(s, "SET LOCAL statement_timeout = 0"):
			if setLocalIdx == -1 {
				setLocalIdx = i
			}
		}
	}
	require.NotEqual(t, -1, createIdx, "CREATE EXTENSION must be issued; got %v", stream)
	require.NotEqual(t, -1, setLocalIdx, "SET LOCAL must be issued; got %v", stream)
	assert.Less(t, createIdx, setLocalIdx, "CREATE EXTENSION must run before the maintenance tx opens")
}

// TestOpen_SkipExtensionWiring (V5) pins the Options.SkipExtension wiring:
// Open with SkipExtension:true must succeed and produce a working backend
// (schema created without running CREATE EXTENSION). Distinct from
// SkipMigrate, which suppresses all DDL.
func TestOpen_SkipExtensionWiring(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	b, err := Open(ctx, Options{DB: db, Dimension: 4, SkipExtension: true})
	require.NoError(t, err, "Open(SkipExtension)")
	t.Cleanup(func() { _ = b.Close() })

	// Backend is usable end-to-end: create a generation, upsert, search.
	seedOneMessage(t, db)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{1: unitVec(4, 0)})
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "Activate")
	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{})
	require.NoError(t, err, "Search")
	require.Len(t, hits, 1, "expected the one embedded message")
	assert.Equal(t, int64(1), hits[0].MessageID)
}

// TestFusedSearch_EmptyFilterParity (V1) asserts the empty-filter fused
// path returns the same top-k as the equivalent explicit-filter path that
// admits every message. With the empty filter the `filtered` CTE is elided
// and liveness is inlined; the result must be byte-identical in ordering.
func TestFusedSearch_EmptyFilterParity(t *testing.T) {
	f := seedThree(t)

	for _, tc := range []struct {
		name string
		req  vector.FusedRequest
	}{
		{
			name: "hybrid_fts_and_ann",
			req: vector.FusedRequest{
				FTSQuery:   "quantum",
				QueryVec:   unitVec(4, 1),
				Generation: f.gen,
				KPerSignal: 10,
				Limit:      10,
				RRFK:       60,
			},
		},
		{
			name: "ann_only",
			req: vector.FusedRequest{
				QueryVec:   unitVec(4, 0),
				Generation: f.gen,
				KPerSignal: 10,
				Limit:      10,
				RRFK:       60,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Empty filter (elides the `filtered` CTE, inlines liveness).
			emptyReq := tc.req
			emptyReq.Filter = vector.Filter{}
			emptyHits, _, err := f.b.FusedSearch(f.ctx, emptyReq)
			require.NoError(t, err, "FusedSearch empty filter")

			// Explicit all-admitting filter (forces the materialized
			// `filtered` CTE path). All three seeded messages have a
			// source_id in {10, 20}, so this admits exactly the same set.
			allReq := tc.req
			allReq.Filter = vector.Filter{SourceIDs: []int64{10, 20}}
			allHits, _, err := f.b.FusedSearch(f.ctx, allReq)
			require.NoError(t, err, "FusedSearch all-admitting filter")

			assert.Equal(t, fusedIDs(allHits), fusedIDs(emptyHits),
				"empty-filter path must return the same ordered hit set as the all-admitting filter path")
		})
	}
}

func fusedIDs(hits []vector.FusedHit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.MessageID
	}
	return out
}

// TestSearch_FilteredInlineExists (V2) covers the rewritten filtered-ANN
// path that keeps the filter in SQL (inline correlated EXISTS) instead of
// shipping a bigint[] of matching ids. Each case asserts the filtered
// result equals the expected set, including a broad filter that admits
// every message (the case the old id-array shape made expensive).
func TestSearch_FilteredInlineExists(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{
		1: unitVec(4, 0),
		2: unitVec(4, 1),
		3: unitVec(4, 2),
	})

	base := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `
		UPDATE messages
		   SET source_id = CASE id WHEN 1 THEN 10 WHEN 2 THEN 20 ELSE 30 END,
		       has_attachments = (id = 2),
		       sent_at = CASE id
		           WHEN 1 THEN $1::timestamptz
		           WHEN 2 THEN $2::timestamptz
		           ELSE $3::timestamptz
		       END
		 WHERE id IN (1, 2, 3)`,
		base, base.Add(time.Hour), base.Add(2*time.Hour))
	require.NoError(t, err, "seed filter columns")

	yes := true
	for _, tc := range []struct {
		name   string
		filter vector.Filter
		query  []float32
		want   []int64
	}{
		{
			name:   "source filter selects one",
			filter: vector.Filter{SourceIDs: []int64{20}},
			query:  unitVec(4, 1),
			want:   []int64{2},
		},
		{
			name:   "attachment filter selects one",
			filter: vector.Filter{HasAttachment: &yes},
			query:  unitVec(4, 1),
			want:   []int64{2},
		},
		{
			name:   "broad source filter admits all, ranked by ANN",
			filter: vector.Filter{SourceIDs: []int64{10, 20, 30}},
			query:  unitVec(4, 0), // closest to msg 1
			want:   []int64{1, 2, 3},
		},
		{
			name:   "no match sentinel",
			filter: vector.Filter{SourceIDs: []int64{999}},
			query:  unitVec(4, 0),
			want:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := b.Search(ctx, gen, tc.query, 10, tc.filter)
			require.NoError(t, err, "Search")
			got := hitMessageIDs(hits)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			// For the broad case assert the full ordered set (ANN order:
			// msg 1 closest to the axis-0 query); for selective filters the
			// single expected id.
			if tc.name == "broad source filter admits all, ranked by ANN" {
				assert.Equal(t, tc.want, got, "broad filter must return all messages in ANN order")
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// TestSearch_FilteredInlineExists_MultiChunk (V2) guards that the rewritten
// filtered path still widens correctly across a multi-chunk filtered
// universe — the ceiling recompute uses the same EXISTS predicate, so the
// inner LIMIT loop reaches k distinct messages rather than short-returning.
func TestSearch_FilteredInlineExists_MultiChunk(t *testing.T) {
	b, ctx, db := newBackendForTest(t)
	for _, id := range []int64{1, 2} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id, source_id) VALUES ($1, 10) ON CONFLICT (id) DO UPDATE SET source_id = 10`, id)
		require.NoErrorf(t, err, "seed msg %d", id)
	}
	// msg 1 contributes two chunks (one close, one far); msg 2 single chunk.
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: unitVec(4, 0)},
		{MessageID: 1, ChunkIndex: 1, Vector: unitVec(4, 2)},
		{MessageID: 2, ChunkIndex: 0, Vector: unitVec(4, 1)},
	}), "Upsert")
	_, err = b.db.ExecContext(ctx, `DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen))
	require.NoError(t, err, "clear pending")

	hits, err := b.Search(ctx, gen, unitVec(4, 0), 10, vector.Filter{SourceIDs: []int64{10}})
	require.NoError(t, err, "Search")
	got := hitMessageIDs(hits)
	// Both messages surface exactly once; msg 1 wins on its close chunk.
	require.Len(t, got, 2, "want both messages once each; got %v", got)
	assert.Equal(t, int64(1), got[0], "msg 1 ranks first on its close chunk")
	assert.ElementsMatch(t, []int64{1, 2}, got, "both filtered messages must appear")
}
