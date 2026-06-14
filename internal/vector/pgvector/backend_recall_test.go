//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// recallDim is large enough to build many distinct near-query vectors so a
// single multi-chunk message can dominate the first inner ANN fetch.
const recallDim = 16

// nearQueryVec returns a unit-ish vector that points mostly along axis 0
// (the query axis) with a tiny perturbation on a second axis, so it ranks
// very close to the query but is distinct from its siblings. Smaller eps
// => closer to the query.
func nearQueryVec(perturbAxis int, eps float64) []float32 {
	v := make([]float32, recallDim)
	v[0] = float32(math.Sqrt(1 - eps*eps))
	if perturbAxis == 0 {
		perturbAxis = 1
	}
	v[perturbAxis%recallDim] = float32(eps)
	return v
}

// midQueryVec returns a vector at a moderate distance from the query: a
// 45°-ish mix of axis 0 and the given axis. These are the single-chunk
// "other" messages that must still surface once the multi-chunk message's
// chunks are deduplicated away.
func midQueryVec(axis int) []float32 {
	v := make([]float32, recallDim)
	v[0] = float32(0.6)
	v[axis%recallDim] = float32(0.8)
	return v
}

// seedRecallCorpus builds a generation where message 1 has many chunks all
// closer to the query than several single-chunk messages. This is the
// pathological multi-chunk shape that defeats a single fixed over-fetch:
// the first inner ANN LIMIT is packed by message 1's chunks, and only the
// candidate-widening loop reaches the single-chunk messages.
//
// Returns the generation id and the query vector (axis 0).
func seedRecallCorpus(t *testing.T, b *Backend, db *sql.DB, multiChunks, singles int) (vector.GenerationID, []float32) {
	t.Helper()
	ctx := context.Background()

	// Insert message rows: id 1 is the multi-chunk message; 2..singles+1 are
	// the single-chunk messages.
	total := 1 + singles
	for id := 1; id <= total; id++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}

	gen, err := b.CreateGeneration(ctx, "m", recallDim, "")
	require.NoError(t, err, "CreateGeneration")

	chunks := make([]vector.Chunk, 0, multiChunks+singles)
	// Message 1: multiChunks chunks, all very close to the query.
	for i := range multiChunks {
		chunks = append(chunks, vector.Chunk{
			MessageID:  1,
			ChunkIndex: i,
			Vector:     nearQueryVec(i+1, 0.01),
		})
	}
	// Single-chunk messages at a moderate distance.
	for j := range singles {
		chunks = append(chunks, vector.Chunk{
			MessageID:  int64(2 + j),
			ChunkIndex: 0,
			Vector:     midQueryVec(j + 2),
		})
	}
	require.NoError(t, b.Upsert(ctx, gen, chunks), "Upsert")

	_, err = b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen))
	require.NoError(t, err, "clear pending")

	query := make([]float32, recallDim)
	query[0] = 1
	return gen, query
}

// TestBackend_Search_MultiChunkCorpus_ReturnsKDistinct asserts that Search
// returns the full k distinct messages even when one message contributes
// far more than annOverFetchFactor chunks, all of which sit ahead of the
// single-chunk messages in ANN order. Without the candidate-widening loop
// the first fixed over-fetch (k*annOverFetchFactor) collapses to a single
// distinct message after GROUP BY and Search short-returns.
func TestBackend_Search_MultiChunkCorpus_ReturnsKDistinct(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	// message 1 contributes many chunks (>> annOverFetchFactor); plus k-1
	// single-chunk messages so the full result set is exactly k distinct.
	const k = 5
	const multiChunks = 40 // far exceeds annOverFetchFactor (4)
	const singles = k - 1
	gen, query := seedRecallCorpus(t, b, db, multiChunks, singles)

	hits, err := b.Search(ctx, gen, query, k, vector.Filter{})
	require.NoError(t, err, "Search")
	require.Len(t, hits, k, "Search must return k distinct messages despite the multi-chunk message")

	seen := map[int64]int{}
	for _, h := range hits {
		seen[h.MessageID]++
	}
	require.Len(t, seen, k, "hits must be k distinct messages")
	for id, n := range seen {
		assert.Equalf(t, 1, n, "message %d returned %d times, want exactly 1", id, n)
	}
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit is the multi-chunk message (its chunks are closest)")
}

// TestBackend_Search_Filtered_MultiChunkCorpus_ReturnsKDistinct asserts
// the same recall guard on the FILTERED Search path (a structured filter
// that matches every seeded row, so it exercises the filtered branch
// rather than the empty-filter fast path). All seeded messages have
// has_attachments = false (the column default), so HasAttachment=false
// matches the entire corpus. Without a chunk-count loop ceiling the
// filtered widening loop caps the inner LIMIT at the filtered MESSAGE
// count (5), saturating before the multi-chunk message's 40 chunks are
// deduplicated, and Search short-returns a single distinct message.
func TestBackend_Search_Filtered_MultiChunkCorpus_ReturnsKDistinct(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	const k = 5
	const multiChunks = 40 // far exceeds annOverFetchFactor (4)
	const singles = k - 1
	gen, query := seedRecallCorpus(t, b, db, multiChunks, singles)

	// Filter matches all rows (has_attachments defaults to false) yet is
	// non-empty, so Search takes the filtered branch.
	no := false
	hits, err := b.Search(ctx, gen, query, k, vector.Filter{HasAttachment: &no})
	require.NoError(t, err, "Search")
	require.Len(t, hits, k, "filtered Search must return k distinct messages despite the multi-chunk message")

	seen := map[int64]int{}
	for _, h := range hits {
		seen[h.MessageID]++
	}
	require.Len(t, seen, k, "filtered hits must be k distinct messages")
	for id, n := range seen {
		assert.Equalf(t, 1, n, "message %d returned %d times, want exactly 1", id, n)
	}
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit is the multi-chunk message (its chunks are closest)")
}

// TestBackend_FusedSearch_MultiChunkCorpus_ReturnsKDistinct mirrors the
// recall guard on the fused (hybrid) ANN path: the ann_pool widening loop
// must reach KPerSignal+1 distinct messages even when a single message's
// chunks dominate the inner ANN scan.
func TestBackend_FusedSearch_MultiChunkCorpus_ReturnsKDistinct(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	const k = 5
	// multiChunks must exceed the fused loop's initial over-fetch,
	// (KPerSignal+1)*fusedANNChunksPerMessage = (5+1)*8 = 48, so the first
	// pass scans only a fraction of message 1's near-query chunks and dedups
	// to a SINGLE distinct message. The widening loop must then iterate
	// (48 -> 96 -> 192 -> 204) before the moderate-distance single-chunk
	// messages (ids 2..k) surface and the pool reaches k distinct. With a
	// corpus that fit inside the initial over-fetch (e.g. multiChunks=40,
	// total 44 < 48) the first pass already scanned everything and the loop
	// broke immediately, so the assertion below would pass even if the fused
	// widening loop were reverted to a single fixed fetch.
	const multiChunks = 200
	const singles = k - 1 // total chunks = 1*multiChunks + singles = 204 > 48
	gen, query := seedRecallCorpus(t, b, db, multiChunks, singles)
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "ActivateGeneration")

	hits, _, err := b.FusedSearch(ctx, vector.FusedRequest{
		QueryVec:   query,
		Generation: gen,
		KPerSignal: k,
		Limit:      k,
		RRFK:       60,
	})
	require.NoError(t, err, "FusedSearch")
	require.Len(t, hits, k, "FusedSearch must return k distinct messages despite the multi-chunk message")

	seen := map[int64]struct{}{}
	for _, h := range hits {
		seen[h.MessageID] = struct{}{}
	}
	assert.Len(t, seen, k, "fused hits must be k distinct messages")

	// The moderate-distance single-chunk messages (ids 2..k) rank BEHIND
	// message 1's 200 near-query chunks in ANN order, so they can only enter
	// the ann_pool after the widening loop fetches past that chunk block.
	// Asserting every one of them is present makes this test depend on the
	// loop iterating: a single fixed fetch of 48 chunks would surface only
	// message 1 and none of ids 2..k.
	for id := int64(2); id <= int64(1+singles); id++ {
		_, ok := seen[id]
		assert.Truef(t, ok, "moderate-distance single-chunk message %d must surface; "+
			"its presence proves the fused widening loop iterated past message 1's chunk block", id)
	}
	assert.Equal(t, int64(1), hits[0].MessageID, "top hit is the multi-chunk message (its chunks are closest)")
}

// seedDistinctNearQueryCorpus builds a generation of `count` single-chunk
// messages (ids 1..count), each a near-query unit vector at a slightly
// increasing distance, so the messages have a stable distance order and
// every message is a distinct ANN candidate. Unlike seedRecallCorpus (one
// dominant multi-chunk message) this shape is what makes pgvector's HNSW
// ef_search cap observable: the index visits at most ef_search candidates,
// so when count > ef_search the inner ANN scan returns fewer DISTINCT
// messages than the LIMIT requests until ef_search is raised.
//
// Returns the generation id and the query vector (axis 0).
func seedDistinctNearQueryCorpus(t *testing.T, b *Backend, db *sql.DB, count int) (vector.GenerationID, []float32) {
	t.Helper()
	ctx := context.Background()

	for id := 1; id <= count; id++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		require.NoErrorf(t, err, "seed message %d", id)
	}

	gen, err := b.CreateGeneration(ctx, "m", recallDim, "")
	require.NoError(t, err, "CreateGeneration")

	chunks := make([]vector.Chunk, 0, count)
	for id := 1; id <= count; id++ {
		// eps grows with id so distances are distinct and ordered, but all
		// stay close to the query (axis 0). The step is scaled by count so
		// the farthest vector still sits near the query (eps <= ~0.2).
		// perturbAxis cycles through the non-zero axes so vectors are not
		// collinear.
		eps := 0.2 * float64(id) / float64(count)
		chunks = append(chunks, vector.Chunk{
			MessageID:  int64(id),
			ChunkIndex: 0,
			Vector:     nearQueryVec(id, eps),
		})
	}
	require.NoError(t, b.Upsert(ctx, gen, chunks), "Upsert")

	_, err = b.db.ExecContext(ctx,
		`DELETE FROM pending_embeddings WHERE generation_id = $1`, int64(gen))
	require.NoError(t, err, "clear pending")

	// ANALYZE so the planner has row-count statistics; without them it may
	// mis-cost the HNSW index against the btree(+sort) alternatives and the
	// EXPLAIN assertion below becomes flaky.
	_, err = b.db.ExecContext(ctx, `ANALYZE embeddings`)
	require.NoError(t, err, "analyze embeddings")
	_, err = b.db.ExecContext(ctx, `ANALYZE messages`)
	require.NoError(t, err, "analyze messages")

	query := make([]float32, recallDim)
	query[0] = 1
	return gen, query
}

// emptyFilterANNSQL mirrors backend.go's empty-filter ANN statement
// (dimension literal, EXISTS-against-messages liveness, inner ORDER BY <=>
// LIMIT, outer GROUP BY + ORDER BY MIN(distance) + LIMIT). The inner ANN
// subquery (ORDER BY embedding <=> $1 LIMIT $3) is identical to backend.go's
// Search fast path — that inner ORDER BY/LIMIT is what selects the HNSW index.
// The outer LIMIT here is widened to innerLimit (vs production's k) so
// COUNT(DISTINCT message_id) can observe the full ef_search candidate ceiling.
// Both the EXPLAIN assertion and the distinct-count run THIS same SQL so the
// planner makes the same HNSW-index choice for the row the test counts as for
// the plan it inspects — a COUNT(*) wrapper would let the planner rewrite the
// inner LIMIT and pick a different (non-HNSW) plan, breaking plan parity
// between the EXPLAIN and the count.
func emptyFilterANNSQL() string {
	return fmt.Sprintf(`
		SELECT ann.message_id, MIN(ann.distance) AS distance
		  FROM (
		        SELECT e.message_id,
		               (e.embedding::vector(%[1]d)) <=> $1::vector AS distance
		          FROM embeddings e
		         WHERE e.generation_id = $2
		           AND e.dimension = %[1]d
		           AND EXISTS (
		                SELECT 1 FROM messages m
		                 WHERE m.id = e.message_id AND %[2]s)
		         ORDER BY e.embedding::vector(%[1]d) <=> $1::vector
		         LIMIT $3
		       ) ann
		 GROUP BY ann.message_id
		 ORDER BY distance
		 LIMIT $3`, recallDim, store.LiveMessagesWhere("m", true))
}

// innerANNDistinctCount runs the empty-filter ANN statement on a single
// pinned connection (so the session-level enable_seqscan / hnsw.ef_search
// SETs apply) and returns the number of DISTINCT messages it surfaces after
// the GROUP BY dedup. It counts rows in Go rather than via a COUNT(*) wrapper
// so the executed plan is byte-identical to the one explainInnerANN inspects.
func innerANNDistinctCount(t *testing.T, conn *sql.Conn, gen vector.GenerationID, query []float32, innerLimit int) int {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.QueryContext(ctx, emptyFilterANNSQL(), vectorLiteral(query), int64(gen), innerLimit)
	require.NoError(t, err, "inner ANN query")
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id int64
		var dist float64
		require.NoError(t, rows.Scan(&id, &dist), "scan ANN row")
		n++
	}
	require.NoError(t, rows.Err(), "iterate ANN rows")
	return n
}

// TestBackend_Search_HNSWIndexPath_EfSearchContrast drives the ANN search
// through the HNSW index (not the sequential scan the other recall tests
// fall back to) so it actually exercises the pgvector ef_search cap that
// store.HNSWEfSearch is wired to defeat.
//
// Why this test exists: openPGTestDB connects with a bare sql.Open that
// bypasses store.postgresConnConfig, so the test session keeps pgvector's
// default hnsw.ef_search=40. The dim=16 / ~44-row recall corpus is also
// small enough that the planner picks a Seq Scan, so the existing recall
// tests never touch the HNSW index and ef_search is irrelevant to them. A
// regression that broke the ef_search wiring (store.go) or the HNSW
// short-return handling would not be caught.
//
// This test closes that gap deterministically:
//  1. enable_seqscan = off plus a few-thousand-row corpus makes the planner
//     pick the HNSW index for the inner ANN scan; we confirm via EXPLAIN
//     ANALYZE that it uses an HNSW Index Scan, not a Seq/btree scan.
//  2. The default ef_search=40 bounds the HNSW graph traversal, so the inner
//     scan surfaces materially fewer distinct messages than the LIMIT asks
//     for (recall is lost); raising ef_search to store.HNSWEfSearch lifts the
//     cap and restores the full LIMIT of distinct messages. Both sides of
//     that contrast are asserted. (The default-side cap is a graph-dependent
//     number well under the LIMIT, not literally 40 — the assertion is "<
//     LIMIT", not an exact value, so the randomized HNSW build stays green.)
//  3. A wiring guard opens a connection through the real store path
//     (store.OpenPostgresDB) and asserts SHOW hnsw.ef_search reports
//     store.HNSWEfSearch, proving the GUC survives the full Open path (not
//     just the RuntimeParams map that postgres_internal_test.go checks).
func TestBackend_Search_HNSWIndexPath_EfSearchContrast(t *testing.T) {
	b, ctx, db := newBackendForTest(t)

	// The corpus must be large enough that (a) the planner prefers the HNSW
	// index over a btree-index(+sort) scan under enable_seqscan=off (at ~100
	// rows the btree+sort path wins and the index is never exercised) and (b)
	// the inner LIMIT exceeds the number of candidates the HNSW graph reaches
	// under the default ef_search=40, so the cap is observable.
	//
	// innerLimit is chosen empirically: at this corpus shape the default
	// ef_search=40 short-returns to ~150 distinct messages (the graph reaches
	// a bounded neighborhood), while ef_search=store.HNSWEfSearch lifts the
	// cap to the full innerLimit. innerLimit must be < HNSWEfSearch so the
	// raised value, not the LIMIT, is the binding ceiling on the raised side.
	const corpus = 2000
	const innerLimit = 500
	require.Greater(t, store.HNSWEfSearch, innerLimit,
		"raised ef_search must be > innerLimit so the index returns the full LIMIT on the raised side")

	gen, query := seedDistinctNearQueryCorpus(t, b, db, corpus)

	// Pin one connection so SET enable_seqscan / SET hnsw.ef_search stick for
	// every query below (a pooled *sql.DB would scatter them across conns).
	conn, err := db.Conn(ctx)
	require.NoError(t, err, "pin connection")
	defer func() { _ = conn.Close() }()

	_, err = conn.ExecContext(ctx, "SET enable_seqscan = off")
	require.NoError(t, err, "disable seqscan")

	// (1) Prove the inner ANN subquery uses the HNSW index, not a Seq Scan.
	_, err = conn.ExecContext(ctx, "SET hnsw.ef_search = 40")
	require.NoError(t, err, "set default ef_search")
	explain := explainInnerANN(t, conn, gen, query, innerLimit)
	indexName := VectorIndexName(recallDim)
	require.Containsf(t, explain, "Index Scan using "+indexName,
		"inner ANN subquery must use the HNSW index, got plan:\n%s", explain)
	require.NotContainsf(t, explain, "Seq Scan on embeddings",
		"inner ANN subquery must not fall back to a Seq Scan, got plan:\n%s", explain)

	// (2a) Default ef_search=40 caps the HNSW graph traversal, so the inner
	// scan surfaces materially fewer DISTINCT messages than the LIMIT asks
	// for — recall is lost. (The exact cap depends on the randomized graph;
	// it is consistently well under innerLimit, around ~150 for this shape.)
	gotDefault := innerANNDistinctCount(t, conn, gen, query, innerLimit)
	assert.Lessf(t, gotDefault, innerLimit,
		"at default ef_search=40 the HNSW index path must short-return below the inner LIMIT (got %d, limit %d)",
		gotDefault, innerLimit)

	// (2b) Raising ef_search to the wired store.HNSWEfSearch lifts the cap so
	// the inner scan returns the full innerLimit distinct messages: recall is
	// restored. This is the exact failure mode store.go's GUC defends against.
	_, err = conn.ExecContext(ctx, "SET hnsw.ef_search = "+strconv.Itoa(store.HNSWEfSearch))
	require.NoError(t, err, "raise ef_search")
	gotRaised := innerANNDistinctCount(t, conn, gen, query, innerLimit)
	assert.Equalf(t, innerLimit, gotRaised,
		"at ef_search=%d the HNSW index path must surface the full inner LIMIT distinct messages (got %d)",
		store.HNSWEfSearch, gotRaised)
	assert.Greaterf(t, gotRaised, gotDefault,
		"raising ef_search must surface strictly more distinct messages than the default (default=%d raised=%d)",
		gotDefault, gotRaised)

	// (3) Wiring guard: a connection opened through the real store path must
	// actually report the raised GUC, proving postgresConnConfig's
	// RuntimeParam survives to a live session (not just the map asserted by
	// store's TestPostgresConnConfigRuntimeParams).
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	require.NotEmpty(t, dsn, "MSGVAULT_TEST_DB must be set for the wiring guard")
	storeDB, cleanup, err := store.OpenPostgresDB(dsn)
	require.NoError(t, err, "open via store path")
	defer cleanup()
	defer func() { _ = storeDB.Close() }()
	var efSearch string
	require.NoError(t, storeDB.QueryRowContext(ctx, "SHOW hnsw.ef_search").Scan(&efSearch),
		"SHOW hnsw.ef_search on store-opened connection")
	assert.Equal(t, strconv.Itoa(store.HNSWEfSearch), efSearch,
		"store-opened connection must report the raised hnsw.ef_search")
}

// explainInnerANN returns the EXPLAIN (ANALYZE) plan text for the empty-filter
// ANN statement (the exact SQL innerANNDistinctCount executes), run on the
// pinned connection so the session SETs apply.
func explainInnerANN(t *testing.T, conn *sql.Conn, gen vector.GenerationID, query []float32, innerLimit int) string {
	t.Helper()
	ctx := context.Background()
	stmt := "EXPLAIN (ANALYZE, COSTS OFF)" + emptyFilterANNSQL()
	rows, err := conn.QueryContext(ctx, stmt, vectorLiteral(query), int64(gen), innerLimit)
	require.NoError(t, err, "EXPLAIN inner ANN")
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line), "scan EXPLAIN line")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	require.NoError(t, rows.Err(), "iterate EXPLAIN")
	return b.String()
}
