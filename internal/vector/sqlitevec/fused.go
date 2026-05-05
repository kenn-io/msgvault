//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/vector"
)

// Compile-time check that *Backend satisfies vector.FusingBackend.
var _ vector.FusingBackend = (*Backend)(nil)

// FusedSearch runs the single-query hybrid CTE (spec §5.3) by opening a
// fresh connection to the main msgvault.db with vectors.db ATTACHed.
// Filters resolve at the Go layer.
//
// The returned saturated flag indicates that either per-signal pool
// hit the KPerSignal cap. Both branches are over-fetched by one row
// (BM25 via LIMIT KPerSignal+1, ANN via k=KPerSignal+1) and trimmed to
// KPerSignal before fusion. The extra "probe" row exists only so the
// outer query can report whether the pool was full on either side.
func (b *Backend) FusedSearch(ctx context.Context, req vector.FusedRequest) ([]vector.FusedHit, bool, error) {
	if req.QueryVec == nil && req.FTSQuery == "" {
		return nil, false, fmt.Errorf("FusedSearch: neither vector nor FTS query provided")
	}

	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = ?`, int64(req.Generation)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, req.Generation)
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup generation %d: %w", req.Generation, err)
	}
	if req.QueryVec != nil && len(req.QueryVec) != dim {
		return nil, false, fmt.Errorf("%w: query has %d dims, gen has %d",
			vector.ErrDimensionMismatch, len(req.QueryVec), dim)
	}

	conn, err := b.openFusedConn(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = conn.Close() }()

	sourceIDs, err := idsToJSON(req.Filter.SourceIDs)
	if err != nil {
		return nil, false, fmt.Errorf("encode source_ids: %w", err)
	}
	senderGroupSQL, senderGroupArgs, err := senderGroupClauses(req.Filter.SenderGroups)
	if err != nil {
		return nil, false, fmt.Errorf("encode sender_groups: %w", err)
	}
	toGroupSQL, toGroupArgs, err := recipientGroupClauses("to", req.Filter.ToGroups)
	if err != nil {
		return nil, false, fmt.Errorf("encode to_groups: %w", err)
	}
	ccGroupSQL, ccGroupArgs, err := recipientGroupClauses("cc", req.Filter.CcGroups)
	if err != nil {
		return nil, false, fmt.Errorf("encode cc_groups: %w", err)
	}
	bccGroupSQL, bccGroupArgs, err := recipientGroupClauses("bcc", req.Filter.BccGroups)
	if err != nil {
		return nil, false, fmt.Errorf("encode bcc_groups: %w", err)
	}
	labelGroupSQL, labelGroupArgs, err := labelGroupClauses(req.Filter.LabelGroups)
	if err != nil {
		return nil, false, fmt.Errorf("encode label_groups: %w", err)
	}
	var hasAttachment sql.NullBool
	if req.Filter.HasAttachment != nil {
		hasAttachment = sql.NullBool{Valid: true, Bool: *req.Filter.HasAttachment}
	}
	// Format date bounds with the canonical SQLite datetime layout used
	// elsewhere in the repo so boundary comparisons on the text sent_at
	// column agree with the existing SQLite query paths.
	var after, before sql.NullString
	if req.Filter.After != nil {
		after = sql.NullString{Valid: true, String: req.Filter.After.Format(sqliteDatetimeFormat)}
	}
	if req.Filter.Before != nil {
		before = sql.NullString{Valid: true, String: req.Filter.Before.Format(sqliteDatetimeFormat)}
	}
	var largerThan, smallerThan sql.NullInt64
	if req.Filter.LargerThan != nil {
		largerThan = sql.NullInt64{Valid: true, Int64: *req.Filter.LargerThan}
	}
	if req.Filter.SmallerThan != nil {
		smallerThan = sql.NullInt64{Valid: true, Int64: *req.Filter.SmallerThan}
	}
	// SubjectSubstrings all get AND-combined; encode as JSON so the CTE
	// can iterate via json_each and apply one LIKE per term without
	// blowing past the bound-parameter cap.
	var subjectPatterns sql.NullString
	if len(req.Filter.SubjectSubstrings) > 0 {
		patterns := make([]string, len(req.Filter.SubjectSubstrings))
		for i, term := range req.Filter.SubjectSubstrings {
			patterns[i] = "%" + escapeLikeSubject(term) + "%"
		}
		buf, err := json.Marshal(patterns)
		if err != nil {
			return nil, false, fmt.Errorf("encode subject patterns: %w", err)
		}
		subjectPatterns = sql.NullString{Valid: true, String: string(buf)}
	}

	vecTable := "vec." + VectorTableName(dim)

	// The sqlite-vec virtual table requires `k = <int literal>` rather
	// than a bound parameter; we interpolate KPerSignal directly (it is
	// a trusted integer from the caller's request).
	//
	// For saturation detection we pull KPerSignal+1 rows from the BM25
	// CTE and then trim to the first K in bm25_used before fusion. The
	// unused K+1 row exists only so the outer query can report whether
	// the pool was full.
	kPlus1 := req.KPerSignal + 1
	query := fmt.Sprintf(`
WITH
  filtered AS (
    SELECT m.id
      FROM messages m
     WHERE %s
       AND (:source_ids IS NULL OR m.source_id IN (SELECT value FROM json_each(:source_ids)))
       %s
       %s
       %s
       %s
       AND (:has_attachment IS NULL OR m.has_attachments = :has_attachment)
       AND (:after IS NULL OR m.sent_at >= :after)
       AND (:before IS NULL OR m.sent_at < :before)
       AND (:larger_than IS NULL OR m.size_estimate > :larger_than)
       AND (:smaller_than IS NULL OR m.size_estimate < :smaller_than)
       AND (:subject_patterns IS NULL OR NOT EXISTS (
             SELECT 1 FROM json_each(:subject_patterns) sp
              WHERE m.subject IS NULL OR m.subject NOT LIKE sp.value ESCAPE '\'))
       %s
  ),
  bm25 AS (
    SELECT fts.rowid AS message_id,
           fts.rank AS bm25_raw,
           ROW_NUMBER() OVER (ORDER BY fts.rank) AS rnk
      FROM messages_fts fts
      JOIN filtered f ON f.id = fts.rowid
     WHERE :fts_query IS NOT NULL AND messages_fts MATCH :fts_query
     ORDER BY fts.rank
     LIMIT %d
  ),
  bm25_used AS (
    SELECT * FROM bm25 WHERE rnk <= %d
  ),
  ann AS (
    SELECT v.message_id,
           v.distance AS vec_dist,
           ROW_NUMBER() OVER (ORDER BY v.distance) AS rnk
      FROM %s v
     WHERE :query_vec IS NOT NULL
       AND v.generation_id = :gen
       AND v.message_id IN (SELECT id FROM filtered)
       AND v.embedding MATCH :query_vec
       AND k = %d
  ),
  ann_used AS (
    SELECT * FROM ann WHERE rnk <= %d
  ),
  fused AS (
    SELECT COALESCE(b.message_id, v.message_id) AS message_id,
           COALESCE(1.0 / (:rrf_k + b.rnk), 0.0) +
           COALESCE(1.0 / (:rrf_k + v.rnk), 0.0) AS rrf_score,
           b.bm25_raw AS bm25_score,
           CASE WHEN v.vec_dist IS NULL THEN NULL ELSE 1.0 - v.vec_dist END AS vector_score
      FROM bm25_used b
      FULL OUTER JOIN ann_used v USING (message_id)
  )
SELECT message_id, rrf_score, bm25_score, vector_score,
       (SELECT COUNT(*) FROM bm25) AS bm25_pool_size,
       (SELECT COUNT(*) FROM ann)  AS ann_pool_size
  FROM fused
 ORDER BY rrf_score DESC, message_id ASC
 LIMIT :limit
`, store.LiveMessagesWhere("m", true), senderGroupSQL, toGroupSQL, ccGroupSQL, bccGroupSQL, labelGroupSQL,
		kPlus1, req.KPerSignal, vecTable, kPlus1, req.KPerSignal)

	var queryVecArg any
	if req.QueryVec != nil {
		queryVecArg = float32SliceBlob(req.QueryVec)
	}
	var ftsArg any
	if req.FTSQuery != "" {
		ftsArg = req.FTSQuery
	}

	// When the subject boost is active, the SQL LIMIT must not cut
	// boost-eligible candidates out of the result set before Go can
	// re-rank them. SubjectBoost is configurable and unbounded, so a
	// fixed multiple of req.Limit isn't enough — a sufficiently large
	// boost can promote a hit from anywhere in the candidate pool.
	//
	// Fetch the entire `fused` row set instead. `fused` is the FULL
	// OUTER JOIN of bm25_used and ann_used, each capped at KPerSignal,
	// so it has at most 2 × KPerSignal rows regardless of how big the
	// underlying corpus is. Asking for that many gives the boost the
	// full candidate pool to reorder before Go trims to req.Limit.
	sqlLimit := req.Limit
	boostActive := req.SubjectBoost > 1.0 && len(req.SubjectTerms) > 0
	if boostActive {
		sqlLimit = 2 * req.KPerSignal
		if sqlLimit < req.Limit {
			sqlLimit = req.Limit // never under-fetch the requested page
		}
	}

	args := []any{
		sql.Named("source_ids", sourceIDs),
		sql.Named("has_attachment", hasAttachment),
		sql.Named("after", after),
		sql.Named("before", before),
		sql.Named("larger_than", largerThan),
		sql.Named("smaller_than", smallerThan),
		sql.Named("subject_patterns", subjectPatterns),
		sql.Named("fts_query", ftsArg),
		sql.Named("query_vec", queryVecArg),
		sql.Named("gen", int64(req.Generation)),
		sql.Named("rrf_k", req.RRFK),
		sql.Named("limit", sqlLimit),
	}
	args = append(args, senderGroupArgs...)
	args = append(args, toGroupArgs...)
	args = append(args, ccGroupArgs...)
	args = append(args, bccGroupArgs...)
	args = append(args, labelGroupArgs...)

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("fused query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []vector.FusedHit
	// bm25_pool_size and ann_pool_size are correlated subqueries that
	// evaluate to the same value on every row of the result. We only
	// need them once, so capture from the first row (they are
	// otherwise redundantly assigned per-iteration). When the result
	// set is empty, both pools are empty by construction:
	//
	//   fused = bm25_used FULL OUTER JOIN ann_used
	//   bm25_used = bm25 WHERE rnk <= KPerSignal       (rnk starts at 1)
	//   ann_used  = ann  WHERE rnk <= KPerSignal
	//
	// FULL OUTER JOIN of two empty sets is empty, and bm25_used/ann_used
	// cannot be empty unless their parent CTE was — so an empty `fused`
	// implies both pools were empty post-filter, which means saturation
	// is provably false. Default-zero pool sizes are correct in that case.
	var bm25PoolSize, annPoolSize int
	var poolSizeRead bool
	for rows.Next() {
		var h vector.FusedHit
		var bm, vec sql.NullFloat64
		var bmPool, annPool int
		if err := rows.Scan(&h.MessageID, &h.RRFScore, &bm, &vec, &bmPool, &annPool); err != nil {
			return nil, false, fmt.Errorf("scan fused hit: %w", err)
		}
		if !poolSizeRead {
			bm25PoolSize = bmPool
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
		return nil, false, fmt.Errorf("iterate fused hits: %w", err)
	}

	if boostActive {
		b.applySubjectBoost(ctx, hits, req.SubjectTerms, req.SubjectBoost)
		// Trim the over-fetched candidate pool back to the requested
		// limit once boost-aware ordering is final.
		if len(hits) > req.Limit {
			hits = hits[:req.Limit]
		}
	}
	// Saturated if either CTE returned more than KPerSignal rows — the
	// extra "K+1 probe" slot was filled. When there are no fused rows
	// there's no sampled pool_size to read, so report not-saturated by
	// convention.
	saturated := bm25PoolSize > req.KPerSignal || annPoolSize > req.KPerSignal
	return hits, saturated, nil
}

// openFusedConn opens a fresh connection to the main msgvault.db with
// vectors.db ATTACHed under the alias "vec". Caller must Close it.
func (b *Backend) openFusedConn(ctx context.Context) (*sql.DB, error) {
	if b.mainPath == "" {
		return nil, fmt.Errorf("FusedSearch requires MainPath in Options")
	}
	conn, err := sql.Open(DriverName(), b.mainPath)
	if err != nil {
		return nil, fmt.Errorf("open main for fused: %w", err)
	}
	// SQLite ATTACH is per-connection; pin the pool to 1 so every
	// subsequent query reuses the attached connection.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS vec`, b.path); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("attach vectors.db: %w", err)
	}
	return conn, nil
}

// idsToJSON encodes an int64 slice as a JSON array for the json_each
// pattern used in the fused CTE. Returns a NULL NullString when the
// slice is empty, so the SQL can short-circuit with IS NULL.
func idsToJSON(ids []int64) (sql.NullString, error) {
	if len(ids) == 0 {
		return sql.NullString{}, nil
	}
	buf, err := json.Marshal(ids)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal ids: %w", err)
	}
	return sql.NullString{Valid: true, String: string(buf)}, nil
}

// senderGroupClauses produces the SQL fragment and named args for
// repeated `from:` operators. Each group becomes its own clause
// AND'd together, and within a group the message satisfies it via
// `m.sender_id IN (group)` OR a 'from' row in message_recipients
// (the legacy fallback for rows where messages.sender_id is NULL).
// Mirrors the existing SQLite search path in internal/store/api.go,
// which emits one EXISTS per `from:` token at the message level so
// a message with multiple `from` recipients can satisfy multiple
// tokens — e.g. `from:alice from:bob` matches a message whose
// `from` recipients are alice and bob, even though the message has
// only one sender_id.
func senderGroupClauses(groups [][]int64) (string, []any, error) {
	if len(groups) == 0 {
		return "", nil, nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(groups))
	for i, ids := range groups {
		js, err := idsToJSON(ids)
		if err != nil {
			return "", nil, fmt.Errorf("encode sender_grp_%d: %w", i, err)
		}
		if !js.Valid {
			continue
		}
		paramName := fmt.Sprintf("sender_grp_%d", i)
		// Match solely against the 'from' recipient rows so repeated
		// from: groups stay coherent and align with the SQLite FTS
		// path (internal/store/api.go:327-336). Mixing sender_id with
		// recipient-row matches across different tokens would let the
		// vector path satisfy queries the SQLite path rejects.
		fmt.Fprintf(&sb, `
       AND EXISTS (
            SELECT 1 FROM message_recipients mr
             WHERE mr.message_id = m.id
               AND mr.recipient_type = 'from'
               AND mr.participant_id IN (SELECT value FROM json_each(:%s)))`,
			paramName)
		args = append(args, sql.Named(paramName, js))
	}
	return sb.String(), args, nil
}

// recipientGroupClauses produces the SQL fragment and named args for a
// repeated address operator (to/cc/bcc). Each group becomes its own
// EXISTS clause AND'd together, so a query like `to:alice to:bob`
// requires the message to have a 'to' recipient matching alice AND a
// 'to' recipient matching bob. Empty groups yields the empty string
// (no clause appended) so the field is unrestricted.
func recipientGroupClauses(recipientType string, groups [][]int64) (string, []any, error) {
	if len(groups) == 0 {
		return "", nil, nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(groups))
	for i, ids := range groups {
		js, err := idsToJSON(ids)
		if err != nil {
			return "", nil, fmt.Errorf("encode %s_grp_%d: %w", recipientType, i, err)
		}
		// Skip if a group resolved to nil (BuildFilter substitutes
		// the no-match sentinel for empty groups, so this only fires
		// for callers constructing Filter directly with a nil entry).
		if !js.Valid {
			continue
		}
		paramName := fmt.Sprintf("%s_grp_%d", recipientType, i)
		fmt.Fprintf(&sb, `
       AND EXISTS (
             SELECT 1 FROM message_recipients mr
              WHERE mr.message_id = m.id
                AND mr.recipient_type = '%s'
                AND mr.participant_id IN (SELECT value FROM json_each(:%s)))`,
			recipientType, paramName)
		args = append(args, sql.Named(paramName, js))
	}
	return sb.String(), args, nil
}

// labelGroupClauses produces the SQL fragment and named args for
// repeated `label:` operators. Each group becomes its own EXISTS
// clause; the message must have a label in EVERY group. Mirrors
// recipientGroupClauses' shape so the SQL pattern is consistent.
func labelGroupClauses(groups [][]int64) (string, []any, error) {
	if len(groups) == 0 {
		return "", nil, nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(groups))
	for i, ids := range groups {
		js, err := idsToJSON(ids)
		if err != nil {
			return "", nil, fmt.Errorf("encode label_grp_%d: %w", i, err)
		}
		if !js.Valid {
			continue
		}
		paramName := fmt.Sprintf("label_grp_%d", i)
		fmt.Fprintf(&sb, `
       AND EXISTS (
             SELECT 1 FROM message_labels ml
              WHERE ml.message_id = m.id
                AND ml.label_id IN (SELECT value FROM json_each(:%s)))`,
			paramName)
		args = append(args, sql.Named(paramName, js))
	}
	return sb.String(), args, nil
}

// applySubjectBoost multiplies the RRF score of each hit whose
// subject contains any of the supplied (already-lowercased) terms as
// a case-insensitive substring, then re-sorts hits by RRF score so
// the boosted entries float to the top. Sets SubjectBoosted=true on
// the hits that received the multiplier so callers (the API explain
// surface, MCP responses) can report it back to the user.
//
// Subjects are fetched in one batch query against the main DB; the
// fused query intentionally does not include them so we keep the
// CTE column shape stable. Failures in the subject lookup are
// logged-and-ignored — the hits remain in their pre-boost order
// rather than dropping the response.
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
		// Don't fail the whole search on a subject-lookup hiccup; the
		// caller will see unboosted scores and can retry.
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
	// Re-sort by RRF DESC, message_id ASC so post-boost ordering
	// matches the SQL CTE's tiebreak rule.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].RRFScore != hits[j].RRFScore {
			return hits[i].RRFScore > hits[j].RRFScore
		}
		return hits[i].MessageID < hits[j].MessageID
	})
}

// batchGetSubjects loads m.subject for the given ids in a single
// query, returning a map keyed by message_id. Missing or NULL
// subjects are absent from the map (or stored as ""), which the
// caller treats as "no subject to boost".
func (b *Backend) batchGetSubjects(ctx context.Context, ids []int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	// Liveness is already enforced upstream in the `filtered` CTE used
	// for ranking; re-filtering here would silently drop the subject
	// for any hit whose row is soft-deleted between ranking and
	// hydration, leaving the caller with a ranked hit and an empty
	// subject. Hydrate whatever was ranked.
	q := fmt.Sprintf(`SELECT id, COALESCE(subject, '') FROM messages WHERE id IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := b.mainDB.QueryContext(ctx, q, args...)
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
