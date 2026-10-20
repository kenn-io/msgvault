//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/msgvault/internal/vector"
)

// MessageCosines returns, for each listed message embedded in gen, the
// cosine similarity of its closest chunk to queryVec. Search scores a hit as
// 1 - L2 distance, which matches cosine only for unit-length vectors, and the
// embedding client stores provider vectors as they come. UnitLength reports
// that queryVec and every chunk read have length 1. A zero vector scores 0.
// Messages with no chunk in gen are left out.
func (b *Backend) MessageCosines(
	ctx context.Context, gen vector.GenerationID, queryVec []float32, messageIDs []int64,
) (vector.CosineScores, error) {
	queryNorm := norm(queryVec)
	out := vector.CosineScores{Scores: map[int64]float64{}, UnitLength: unitLength(queryNorm)}
	if len(messageIDs) == 0 {
		return out, nil
	}
	dim, err := b.queryDimension(ctx, gen, queryVec)
	if err != nil {
		return vector.CosineScores{}, err
	}
	encoded, err := json.Marshal(messageIDs, json.Deterministic(true))
	if err != nil {
		return vector.CosineScores{}, fmt.Errorf("encode cosine message ids: %w", err)
	}
	rows, err := b.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT e.message_id, v.embedding
		  FROM embeddings e
		  JOIN %s v ON v.embedding_id = e.embedding_id
		 WHERE v.generation_id = ?
		   AND e.message_id IN (SELECT value FROM json_each(?))`, VectorTableName(dim)),
		int64(gen), string(encoded))
	if err != nil {
		return vector.CosineScores{}, fmt.Errorf("load message chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return vector.CosineScores{}, fmt.Errorf("scan chunk row: %w", err)
		}
		vec, err := blobToFloat32(blob, dim)
		if err != nil {
			return vector.CosineScores{}, fmt.Errorf("decode message %d chunk vector: %w", id, err)
		}
		vecNorm := norm(vec)
		out.UnitLength = out.UnitLength && unitLength(vecNorm)
		score := cosine(vec, queryVec, vecNorm, queryNorm)
		if best, seen := out.Scores[id]; !seen || score > best {
			out.Scores[id] = score
		}
	}
	if err := rows.Err(); err != nil {
		return vector.CosineScores{}, fmt.Errorf("iterate chunk rows: %w", err)
	}
	return out, nil
}

// queryDimension returns gen's dimension after checking queryVec has it.
func (b *Backend) queryDimension(ctx context.Context, gen vector.GenerationID, queryVec []float32) (int, error) {
	var dim int
	err := b.db.QueryRowContext(ctx,
		`SELECT dimension FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dim)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return 0, fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if len(queryVec) != dim {
		return 0, fmt.Errorf("%w: query has %d dims, gen has %d",
			vector.ErrDimensionMismatch, len(queryVec), dim)
	}
	return dim, nil
}

// norm returns the length of v.
func norm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}

// unitLength reports that a length is 1, within float32 rounding.
func unitLength(n float64) bool { return math.Abs(n-1) <= 1e-3 }

// cosine returns the cosine similarity of two equal-length vectors with
// lengths normA and normB, or 0 when either is zero.
func cosine(a, b []float32, normA, normB float64) float64 {
	if normA == 0 || normB == 0 {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (normA * normB)
}
