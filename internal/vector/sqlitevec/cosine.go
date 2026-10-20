//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"

	"go.kenn.io/msgvault/internal/vector"
)

// MessageCosines returns, for each listed message embedded in gen, the
// cosine similarity of its closest chunk to queryVec. Search scores a hit as
// 1 - L2 distance, whose ordering matches cosine for unit-length vectors. The
// embedding client stores provider vectors as they come. UnitLength reports
// that queryVec and every chunk read have length 1. A zero vector scores 0.
// Messages with no chunk in gen are left out. queryVec must have gen's
// dimension, as the search that returned the messages checked.
func (b *Backend) MessageCosines(
	ctx context.Context, gen vector.Generation, queryVec []float32, messageIDs []int64,
) (vector.CosineScores, error) {
	queryNorm := norm(queryVec)
	out := vector.CosineScores{Scores: map[int64]float64{}, UnitLength: unitLength(queryNorm)}
	if len(messageIDs) == 0 {
		return out, nil
	}
	dim := gen.Dimension
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
		int64(gen.ID), string(encoded))
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
