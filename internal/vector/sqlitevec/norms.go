//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/vector"
)

// normRange is the shortest and longest nonzero vector a generation holds.
// Zero vectors score 0 against any query, so they are left out.
type normRange struct {
	min, max float64
	any      bool
}

func (r *normRange) add(v []float32) {
	n := norm(v)
	switch {
	case n == 0:
	case !r.any:
		r.min, r.max, r.any = n, n, true
	default:
		r.min, r.max = min(r.min, n), max(r.max, n)
	}
}

// readNormRangeTx returns gen's recorded vector length range and whether it
// is current: recorded at the generation's present vector_revision. Every
// embeddings write moves vector_revision through triggers, so a write that
// did not record the range, such as one from an older binary, leaves it
// stale.
func readNormRangeTx(ctx context.Context, tx *sql.Tx, gen vector.GenerationID) (normRange, bool, error) {
	var low, high sql.NullFloat64
	var current sql.NullBool
	if err := tx.QueryRowContext(ctx, `
		SELECT vector_norm_min, vector_norm_max, vector_norm_revision = vector_revision
		  FROM index_generations WHERE id = ?`, int64(gen)).Scan(&low, &high, &current); err != nil {
		return normRange{}, false, fmt.Errorf("read generation %d vector lengths: %w", gen, err)
	}
	return normRange{min: low.Float64, max: high.Float64, any: low.Valid}, current.Valid && current.Bool, nil
}

// recordNormRangeTx records r as gen's vector length range at the
// generation's present vector_revision.
func recordNormRangeTx(ctx context.Context, tx *sql.Tx, gen vector.GenerationID, r normRange) error {
	var low, high any
	if r.any {
		low, high = r.min, r.max
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE index_generations
		   SET vector_norm_min = ?, vector_norm_max = ?, vector_norm_revision = vector_revision
		 WHERE id = ?`, low, high, int64(gen)); err != nil {
		return fmt.Errorf("record generation %d vector lengths: %w", gen, err)
	}
	return nil
}

// currentNormRangesTx returns the generations in schema, such as "" or
// "vec.", whose vector length range is current.
func currentNormRangesTx(ctx context.Context, tx *sql.Tx, schema string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM `+schema+`index_generations
		WHERE vector_norm_revision = vector_revision`)
	if err != nil {
		return nil, fmt.Errorf("list current vector length ranges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan current vector length range: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list current vector length ranges: %w", err)
	}
	return ids, nil
}

// keepNormRangesTx marks the length ranges of gens current again after a
// delete. Removing vectors cannot widen a range, so one that bounded every
// vector before the delete still bounds those left.
func keepNormRangesTx(ctx context.Context, tx *sql.Tx, schema string, gens []int64) error {
	if len(gens) == 0 {
		return nil
	}
	encoded, err := json.Marshal(gens, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode generation ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+schema+`index_generations
		   SET vector_norm_revision = vector_revision
		 WHERE id IN (SELECT value FROM json_each(?))`, string(encoded)); err != nil {
		return fmt.Errorf("keep vector length ranges: %w", err)
	}
	return nil
}

// UnretrievedCosineBound returns the highest cosine similarity to queryVec
// that a vector of gen left out of a search's vector pool could have. SQLite
// ranks by L2 distance, so a vector left out lies at least as far from
// queryVec as the pool's farthest hit, whose score edgeScore is 1 - L2
// distance. A far vector can still point the query's way when it is long, so
// the bound depends on the generation's range of vector lengths. Every write
// path keeps that range for generations this binary created. When it is
// unknown or stale, as for a generation created or written by an older
// binary, any vector may be left out and the bound is 1; reading every vector
// through vec0 to learn the range is too slow for a request. Like pgvector's
// HNSW index, an approximate accelerator can miss nearer vectors; the bound
// trusts the index's order.
func (b *Backend) UnretrievedCosineBound(
	ctx context.Context, gen vector.Generation, queryVec []float32, edgeScore float64,
) (float64, error) {
	queryNorm := norm(queryVec)
	if queryNorm == 0 {
		return 0, nil
	}
	// A query-only open skips migration, so an older vectors.db may lack
	// the columns.
	tracked, err := columnExists(ctx, b.db, "index_generations", "vector_norm_revision")
	if err != nil || !tracked {
		return 1, err
	}
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("begin vector length read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	lengths, current, err := readNormRangeTx(ctx, tx, gen.ID)
	switch {
	case err != nil:
		return 0, err
	case !current:
		return 1, nil
	case !lengths.any:
		return 0, nil
	}
	// Distances come from float32 arithmetic, so shrink the edge slightly
	// rather than let rounding put a left-out vector inside it.
	distance := max(0, 1-edgeScore) * (1 - 1e-4)
	return cosineBeyond(queryNorm, distance, lengths), nil
}

// cosineBeyond bounds the cosine between a query of length s and a vector,
// with length l in r, at L2 distance at least d from it. From
// |q - v|² = s² + l² - 2 q·v ≥ d², the cosine is at most
// (s² + l² - d²) / (2sl), which over l in r peaks at an end of the range.
func cosineBeyond(s, d float64, r normRange) float64 {
	at := func(l float64) float64 { return (s*s + l*l - d*d) / (2 * s * l) }
	return min(1, max(at(r.min), at(r.max)))
}
