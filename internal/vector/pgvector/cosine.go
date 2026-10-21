//go:build pgvector

package pgvector

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/vector"
)

// CosineSearchOrderGuaranteed reports that vector retrieval uses cosine
// distance as its ranking metric.
func (*Backend) CosineSearchOrderGuaranteed() bool { return true }

// MessageCosines returns each embedded message's best chunk cosine in gen.
// Messages without chunks are omitted. Zero vectors score 0.
// queryVec must have gen's dimension, as checked by the preceding search.
func (b *Backend) MessageCosines(
	ctx context.Context, gen vector.Generation, queryVec []float32, messageIDs []int64,
) (vector.CosineScores, error) {
	out := vector.CosineScores{Scores: map[int64]float64{}}
	if len(messageIDs) == 0 {
		return out, nil
	}
	// pgvector returns NaN for cosine distance when either vector is zero.
	rows, err := b.db.QueryContext(ctx, `
		SELECT message_id,
		       MAX(COALESCE(NULLIF(1 - (embedding <=> $2::vector), 'NaN'::float8), 0))
		  FROM embeddings
		 WHERE generation_id = $1 AND message_id = ANY($3::bigint[])
		 GROUP BY message_id`, int64(gen.ID), vectorLiteral(queryVec), int64Array(messageIDs))
	if err != nil {
		return vector.CosineScores{}, fmt.Errorf("score message cosines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return vector.CosineScores{}, fmt.Errorf("scan message cosine: %w", err)
		}
		out.Scores[id] = score
	}
	if err := rows.Err(); err != nil {
		return vector.CosineScores{}, fmt.Errorf("iterate message cosines: %w", err)
	}
	return out, nil
}
