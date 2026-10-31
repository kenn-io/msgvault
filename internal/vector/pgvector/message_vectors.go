//go:build pgvector

package pgvector

import (
	"context"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/vector"
)

var _ vector.MessageVectorReader = (*Backend)(nil)

// MessageVectors reads bounded, ordered chunks from the requested generation.
func (b *Backend) MessageVectors(ctx context.Context, gen vector.Generation, messageIDs []int64, maxChunks int) (map[int64][][]float32, error) {
	ids, err := vector.MessageVectorIDs(gen, messageIDs, maxChunks)
	if err != nil {
		return nil, err
	}
	result := make(map[int64][][]float32, len(ids))
	for page := range slices.Chunk(ids, vector.FilteredCoverageBatchSize) {
		if err := b.readMessageVectorPage(ctx, gen, page, maxChunks, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (b *Backend) readMessageVectorPage(ctx context.Context, gen vector.Generation, ids []int64, maxChunks int, result map[int64][][]float32) error {
	rows, err := b.db.QueryContext(ctx, `
		SELECT message_id, chunk_index, embedding::text
		  FROM embeddings
		 WHERE generation_id = $1 AND message_id = ANY($2::bigint[])
		   AND chunk_index < $3
		 ORDER BY message_id, chunk_index`, int64(gen.ID), int64Array(ids), maxChunks)
	if err != nil {
		return fmt.Errorf("load message vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var chunkIndex int
		var lit string
		if err := rows.Scan(&id, &chunkIndex, &lit); err != nil {
			return fmt.Errorf("scan message vector: %w", err)
		}
		v, err := parseVectorLiteral(lit, gen.Dimension)
		if err != nil {
			return fmt.Errorf("decode message %d chunk %d vector: %w", id, chunkIndex, err)
		}
		result[id] = append(result[id], v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate message vectors: %w", err)
	}
	return nil
}
