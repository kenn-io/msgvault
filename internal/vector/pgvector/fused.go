//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// Compile-time check that *Backend satisfies vector.FusingBackend.
var _ vector.FusingBackend = (*Backend)(nil)

// fusedANNChunksPerMessage bounds how many chunks one message may
// contribute to the inner ANN scan, sizing the inner LIMIT so the
// outer GROUP BY still yields enough distinct messages.
const fusedANNChunksPerMessage = 8

// FusedSearch runs the single-query hybrid CTE against pgvector.
// Mirrors sqlitevec.FusedSearch (spec §5.3) but built around
// to_tsquery + ts_rank_cd on the inline messages.search_fts
// column and pgvector's `<=>` cosine-distance operator. The FTS
// argument is rendered from req.FTSTerms via
// query.PostgreSQLQueryDialect.BuildFTSTerm (lexeme:* AND-joined) so
// the BM25 leg prefix-matches the same term set SQLite does. The
// 'simple' text-search configuration matches what
// FTSUpsert/FTSBackfillBatchSQL in internal/store/dialect_pg.go writes
// into search_fts, so query-time tokens line up with stored tokens (no
// English stemming on either side). The returned saturated flag is true
// when either per-signal pool produced more than KPerSignal candidates —
// the pool was capped and downstream callers should consider raising
// KPerSignal or narrowing the query.
func (b *Backend) FusedSearch(ctx context.Context, req vector.FusedRequest) ([]vector.FusedHit, bool, error) {
	useFTS := len(req.FTSTerms) > 0
	useANN := req.QueryVec != nil
	if !useFTS && !useANN {
		return nil, false, errors.New("FusedSearch: neither vector nor FTS query provided")
	}

	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = $1`, int64(req.Generation)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, req.Generation)
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup generation %d: %w", req.Generation, err)
	}
	if useANN && len(req.QueryVec) != dim {
		return nil, false, fmt.Errorf("%w: query has %d dims, gen has %d",
			vector.ErrDimensionMismatch, len(req.QueryVec), dim)
	}

	// When the subject boost is active, the SQL LIMIT must not cut
	// boost-eligible candidates out of the result set before Go can
	// re-rank them. Fetch the entire fused candidate pool (at most
	// 2 × KPerSignal rows) so the boost has the full pool to reorder.
	sqlLimit := req.Limit
	boostActive := req.SubjectBoost > 1.0 && len(req.SubjectTerms) > 0
	if boostActive {
		sqlLimit = max(2*req.KPerSignal, req.Limit)
	}

	kPlus1 := req.KPerSignal + 1

	// Candidate-widening bounds for the ANN pool. The inner ANN scan over-
	// fetches chunks so the outer GROUP BY can still surface KPerSignal+1
	// distinct messages; on multi-chunk corpora a few long messages can
	// pack the inner LIMIT and collapse the deduplicated pool below that.
	// chunkCeiling (generation-wide chunk count) is the hard upper bound so
	// the widening loop always terminates; filteredCeiling (distinct
	// filtered messages with at least one chunk) is an early exit so a
	// selective filter does not drive the loop to the full generation.
	// Both are only needed on the ANN side.
	var chunkCeiling, filteredCeiling int
	if useANN {
		if chunkCeiling, err = b.chunkCount(ctx, req.Generation); err != nil {
			return nil, false, err
		}
		if filteredCeiling, err = b.filteredChunkMessageCount(ctx, req.Generation, req.Filter); err != nil {
			return nil, false, err
		}
	}

	// runFused builds and executes the fused CTE for a given inner ANN
	// LIMIT (innerChunks) and returns the hits plus the per-signal pool
	// sizes. Args, binds and CTEs are rebuilt per call because the inner
	// LIMIT participates in positional binding; the widening loop re-issues
	// it with a growing innerChunks until the ANN pool is large enough.
	runFused := func(innerChunks int) ([]vector.FusedHit, int, int, error) {
		var args []any
		bind := func(v any) string {
			args = append(args, v)
			return fmt.Sprintf("$%d", len(args))
		}

		liveSQL := store.LiveMessagesWhere("m", true)
		emptyFilter := req.Filter.IsEmpty()

		// When the structured filter is empty the candidate universe is the
		// whole live-message set, so materializing a `filtered AS (SELECT
		// m.id FROM messages m WHERE <live>)` CTE (referenced by both signal
		// CTEs) would force PG to build the entire 1M-row live universe per
		// hybrid query. Mirror the non-fused Search empty-filter fast path
		// (backend.go ~816-818): drop the `filtered` CTE and inline the
		// liveness predicate into each signal — fts_pool already scans
		// `messages m` so it just appends `AND <live>`; the ann_pool inner
		// subquery uses a correlated EXISTS instead of a JOIN. For a
		// non-empty filter the materialized CTE is reasonable: it computes
		// the filtered set once and reuses it across both signals. [V1]
		var ctes []string
		if !emptyFilter {
			filterSQL := applyFilterClauses(req.Filter, bind)
			ctes = append(ctes, fmt.Sprintf(
				"filtered AS (SELECT m.id FROM messages m WHERE %s%s)", liveSQL, filterSQL))
		}

		if useFTS {
			// Render the dialect-neutral terms into a PG tsquery arg
			// (lexeme:* AND-joined) BEFORE binding. to_tsquery requires a
			// pre-formatted argument (operators &, prefix :*) and ERRORS on
			// raw text — this is safe ONLY because we always feed it
			// PostgreSQLQueryDialect.BuildFTSTerm's output, never raw text.
			_, tsArg := query.PostgreSQLQueryDialect{}.BuildFTSTerm(req.FTSTerms)
			ftsArg := bind(tsArg)
			kp1Arg := bind(kPlus1)
			kArg := bind(req.KPerSignal)
			// Empty filter: scan messages directly with an inline liveness
			// predicate. Non-empty filter: intersect with the materialized
			// `filtered` set via JOIN.
			ftsFrom := "      JOIN filtered f ON f.id = m.id\n"
			ftsLive := ""
			if emptyFilter {
				ftsFrom = ""
				ftsLive = "       AND " + liveSQL + "\n"
			}
			ctes = append(ctes,
				fmt.Sprintf(`fts_pool AS (
    SELECT m.id AS message_id,
           ts_rank_cd(m.search_fts, to_tsquery('simple', %s), 32) AS bm25
      FROM messages m
%s     WHERE m.search_fts @@ to_tsquery('simple', %s)
%s     ORDER BY bm25 DESC
     LIMIT %s
)`, ftsArg, ftsFrom, ftsArg, ftsLive, kp1Arg),
				fmt.Sprintf(`fts_ranked AS (
    SELECT message_id, bm25,
           ROW_NUMBER() OVER (ORDER BY bm25 DESC, message_id ASC) AS rnk
      FROM fts_pool
     ORDER BY bm25 DESC, message_id ASC
     LIMIT %s
)`, kArg))
		}

		if useANN {
			vecArg := bind(vectorLiteral(req.QueryVec))
			genArg := bind(int64(req.Generation))
			innerArg := bind(innerChunks)
			kp1Arg := bind(kPlus1)
			kArg := bind(req.KPerSignal)
			// Use an inner SELECT with ORDER BY <=> LIMIT so pgvector can
			// apply the HNSW index before the outer GROUP BY collapses
			// multi-chunk messages. The candidate set is constrained either
			// by an inline correlated EXISTS against the live-message set
			// (empty filter — mirrors backend.go ~816-818, lets the HNSW
			// index drive the inner ORDER BY without first materializing the
			// whole universe) or by a JOIN to the materialized `filtered`
			// CTE (non-empty filter; note that JOIN may, depending on
			// PG/pgvector version and planner costing, force a sequential
			// ANN scan within the filtered set — same tradeoff as
			// backend.go's filtered path, not yet verified with EXPLAIN
			// ANALYZE). The outer GROUP BY then picks the best-scoring chunk
			// per message via MIN(distance) and limits to KPerSignal+1
			// distinct messages, so the outer LIMIT is applied after dedup.
			// The widening loop in FusedSearch grows innerChunks when this
			// dedup collapses the pool below KPerSignal+1.
			annConstraint := "              JOIN filtered f ON f.id = e.message_id\n"
			annLive := ""
			if emptyFilter {
				annConstraint = ""
				annLive = fmt.Sprintf(
					"               AND EXISTS (SELECT 1 FROM messages m WHERE m.id = e.message_id AND %s)\n",
					liveSQL)
			}
			ctes = append(ctes,
				fmt.Sprintf(`ann_pool AS (
    SELECT ann.message_id,
           MIN(ann.distance) AS distance
      FROM (
            SELECT e.message_id,
                   (e.embedding::vector(%[1]d)) <=> %[2]s::vector AS distance
              FROM embeddings e
%[6]s             WHERE e.generation_id = %[3]s AND e.dimension = %[1]d
%[7]s             ORDER BY e.embedding::vector(%[1]d) <=> %[2]s::vector
             LIMIT %[4]s
           ) ann
     GROUP BY ann.message_id
     ORDER BY distance
     LIMIT %[5]s
)`, dim, vecArg, genArg, innerArg, kp1Arg, annConstraint, annLive),
				fmt.Sprintf(`ann_ranked AS (
    SELECT message_id, distance,
           ROW_NUMBER() OVER (ORDER BY distance ASC, message_id ASC) AS rnk
      FROM ann_pool
     ORDER BY distance ASC, message_id ASC
     LIMIT %s
)`, kArg))
		}

		// Pool CTEs are now fully bound. Remember how many args belong to
		// the pool prefix so the empty-result saturation fallback can
		// re-run the prefix-only SQL without the trailing rrfk/limit args.
		poolArgsLen := len(args)
		poolCTEs := append([]string(nil), ctes...)

		rrfkArg := bind(req.RRFK)
		limitArg := bind(sqlLimit)

		var fusedSQL string
		switch {
		case useFTS && useANN:
			fusedSQL = fmt.Sprintf(`fused AS (
    SELECT COALESCE(b.message_id, v.message_id) AS message_id,
           COALESCE(1.0 / (%s + b.rnk), 0.0) +
           COALESCE(1.0 / (%s + v.rnk), 0.0) AS rrf_score,
           b.bm25 AS bm25_score,
           CASE WHEN v.distance IS NULL THEN NULL ELSE 1.0 - v.distance END AS vector_score
      FROM fts_ranked b
      FULL OUTER JOIN ann_ranked v USING (message_id)
)`, rrfkArg, rrfkArg)
		case useFTS:
			fusedSQL = fmt.Sprintf(`fused AS (
    SELECT b.message_id AS message_id,
           1.0 / (%s + b.rnk) AS rrf_score,
           b.bm25 AS bm25_score,
           CAST(NULL AS DOUBLE PRECISION) AS vector_score
      FROM fts_ranked b
)`, rrfkArg)
		case useANN:
			fusedSQL = fmt.Sprintf(`fused AS (
    SELECT v.message_id AS message_id,
           1.0 / (%s + v.rnk) AS rrf_score,
           CAST(NULL AS DOUBLE PRECISION) AS bm25_score,
           1.0 - v.distance AS vector_score
      FROM ann_ranked v
)`, rrfkArg)
		}
		ctes = append(ctes, fusedSQL)

		ftsPoolExpr := "0"
		annPoolExpr := "0"
		if useFTS {
			ftsPoolExpr = "(SELECT COUNT(*) FROM fts_pool)"
		}
		if useANN {
			annPoolExpr = "(SELECT COUNT(*) FROM ann_pool)"
		}

		query := "WITH " + strings.Join(ctes, ",\n") + fmt.Sprintf(`
SELECT message_id, rrf_score, bm25_score, vector_score,
       %s AS fts_pool_size,
       %s AS ann_pool_size
  FROM fused
 ORDER BY rrf_score DESC, message_id ASC
 LIMIT %s`, ftsPoolExpr, annPoolExpr, limitArg)

		rows, err := b.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("fused query: %w", err)
		}
		defer func() { _ = rows.Close() }()

		var hits []vector.FusedHit
		var ftsPoolSize, annPoolSize int
		var poolSizeRead bool
		for rows.Next() {
			var h vector.FusedHit
			var bm, vec sql.NullFloat64
			var ftsPool, annPool int
			if err := rows.Scan(&h.MessageID, &h.RRFScore, &bm, &vec, &ftsPool, &annPool); err != nil {
				return nil, 0, 0, fmt.Errorf("scan fused hit: %w", err)
			}
			if !poolSizeRead {
				ftsPoolSize = ftsPool
				annPoolSize = annPool
				poolSizeRead = true
			}
			h.BM25Score = math.NaN()
			if bm.Valid {
				h.BM25Score = bm.Float64
			}
			h.VectorScore = math.NaN()
			if vec.Valid {
				h.VectorScore = vec.Float64
			}
			hits = append(hits, h)
		}
		if err := rows.Err(); err != nil {
			return nil, 0, 0, fmt.Errorf("iterate fused hits: %w", err)
		}

		// Saturation: when the fused result set is empty, the correlated
		// subqueries that carry pool counts never fire (they ride on the
		// row stream). Fall back to a prefix-only query that uses just
		// the pool CTEs and their args — the trailing rrfk/limit args
		// are excluded so PG's "expected N arguments" check is satisfied.
		if !poolSizeRead {
			prefix := "WITH " + strings.Join(poolCTEs, ",\n") + "\n"
			prefixArgs := args[:poolArgsLen]
			if useFTS {
				if err := b.db.QueryRowContext(ctx,
					prefix+"SELECT COUNT(*) FROM fts_pool", prefixArgs...).Scan(&ftsPoolSize); err != nil {
					return nil, 0, 0, fmt.Errorf("count fts_pool: %w", err)
				}
			}
			if useANN {
				if err := b.db.QueryRowContext(ctx,
					prefix+"SELECT COUNT(*) FROM ann_pool", prefixArgs...).Scan(&annPoolSize); err != nil {
					return nil, 0, 0, fmt.Errorf("count ann_pool: %w", err)
				}
			}
		}
		return hits, ftsPoolSize, annPoolSize, nil
	}

	// Widening loop. Start at (KPerSignal+1)*fusedANNChunksPerMessage so the
	// common single-chunk case is a single query. Grow innerChunks (doubling,
	// bounded by chunkCeiling) while the ANN pool dedups below KPerSignal+1
	// and more chunks remain to scan. The FTS side never collapses (one row
	// per message_id), so only the ANN pool drives widening. When useANN is
	// false the loop runs exactly once.
	var (
		hits        []vector.FusedHit
		ftsPoolSize int
		annPoolSize int
	)
	innerChunks := kPlus1 * fusedANNChunksPerMessage
	for {
		if useANN && chunkCeiling > 0 && innerChunks > chunkCeiling {
			innerChunks = chunkCeiling
		}
		hits, ftsPoolSize, annPoolSize, err = runFused(innerChunks)
		if err != nil {
			return nil, false, err
		}
		if !useANN ||
			annPoolSize >= kPlus1 ||
			annPoolSize >= filteredCeiling ||
			innerChunks >= chunkCeiling {
			break
		}
		next := innerChunks * 2
		if chunkCeiling > 0 && next > chunkCeiling {
			next = chunkCeiling
		}
		if next == innerChunks {
			break
		}
		innerChunks = next
	}

	if boostActive {
		b.applySubjectBoost(ctx, hits, req.SubjectTerms, req.SubjectBoost)
		if len(hits) > req.Limit {
			hits = hits[:req.Limit]
		}
	}

	saturated := ftsPoolSize > req.KPerSignal || annPoolSize > req.KPerSignal
	return hits, saturated, nil
}

// filteredChunkMessageCount returns the number of distinct messages that
// (a) have at least one chunk in the generation and (b) satisfy the live
// + structured filter. It is the early-exit ceiling for the ANN-pool
// widening loop: once the pool has surfaced this many distinct messages,
// every filtered message with a chunk is already in the pool with its
// best-distance chunk, so further widening can neither add a message nor
// change a ranking. The WHERE clause is assembled from the same
// LiveMessagesWhere + buildPGFilterClauses builders the fused query uses,
// so the ceiling predicate stays aligned with the `filtered` CTE.
func (b *Backend) filteredChunkMessageCount(ctx context.Context, gen vector.GenerationID, f vector.Filter) (int, error) {
	args := []any{int64(gen)}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	clauses := append([]string{store.LiveMessagesWhere("m", true)}, buildPGFilterClauses(f, bind)...)
	query := `SELECT COUNT(DISTINCT e.message_id)
	            FROM embeddings e
	            JOIN messages m ON m.id = e.message_id
	           WHERE e.generation_id = $1 AND ` + strings.Join(clauses, " AND ")
	var n int
	if err := b.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("filtered chunk message count: %w", err)
	}
	return n, nil
}

// applyFilterClauses returns the " AND ..." fragment to append after a
// LiveMessagesWhere predicate so each WHERE in the fused CTE narrows
// down to messages matching the structured filter. Delegates to
// buildPGFilterFragment (filter.go) so the clause logic stays in one
// place and evolves consistently across the fast-path and fused paths.
func applyFilterClauses(f vector.Filter, bind func(any) string) string {
	return buildPGFilterFragment(f, bind)
}

// applySubjectBoost re-ranks hits whose subject contains any of the
// supplied (already-lowercased) terms as a case-insensitive
// substring. Mirrors sqlitevec.applySubjectBoost; a failed subject
// lookup degrades gracefully to "unboosted ordering" rather than
// failing the search.
func (b *Backend) applySubjectBoost(ctx context.Context, hits []vector.FusedHit, subjectTerms []string, boost float64) {
	if len(hits) == 0 || len(subjectTerms) == 0 || boost <= 1.0 {
		return
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.MessageID
	}
	subjects, err := b.batchGetSubjects(ctx, ids)
	if err != nil {
		slog.Default().Warn("pgvector: applySubjectBoost: subject hydration failed, returning unboosted order", "err", err)
		return
	}
	for i := range hits {
		subj := subjects[hits[i].MessageID]
		if subj == "" {
			continue
		}
		lower := strings.ToLower(subj)
		for _, term := range subjectTerms {
			if term == "" {
				continue
			}
			if strings.Contains(lower, term) {
				hits[i].RRFScore *= boost
				hits[i].SubjectBoosted = true
				break
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].RRFScore != hits[j].RRFScore {
			return hits[i].RRFScore > hits[j].RRFScore
		}
		return hits[i].MessageID < hits[j].MessageID
	})
}

// batchGetSubjects loads m.subject for the given ids in one query.
// Liveness is enforced upstream during ranking, so we hydrate
// whatever was ranked without re-filtering — re-filtering here would
// silently drop the subject for hits soft-deleted between ranking
// and hydration.
func (b *Backend) batchGetSubjects(ctx context.Context, ids []int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return nil, nil //nolint:nilnil // empty input → no subjects and no error; callers range the map
	}
	rows, err := b.db.QueryContext(ctx,
		`SELECT id, COALESCE(subject, '') FROM messages WHERE id = ANY($1::bigint[])`,
		int64Array(ids))
	if err != nil {
		return nil, fmt.Errorf("batch get subjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var subj string
		if err := rows.Scan(&id, &subj); err != nil {
			return nil, fmt.Errorf("scan subject: %w", err)
		}
		out[id] = subj
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate subjects: %w", err)
	}
	return out, nil
}
