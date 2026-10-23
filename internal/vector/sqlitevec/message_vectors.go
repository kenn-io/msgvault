//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/vector"
)

var _ vector.MessageVectorReader = (*Backend)(nil)

// MessageVectors reads bounded, ordered chunks from the requested generation.
func (b *Backend) MessageVectors(ctx context.Context, gen vector.Generation, messageIDs []int64, maxChunks int) (map[int64][][]float32, error) {
	result := make(map[int64][][]float32)
	if len(messageIDs) == 0 {
		return result, nil
	}
	if len(messageIDs) > vector.MaxFilterMessageIDs {
		return nil, fmt.Errorf("%w: got %d", vector.ErrFilterTooLarge, len(messageIDs))
	}
	if maxChunks < 1 || gen.Dimension < 1 {
		return nil, fmt.Errorf("message vectors require positive maxChunks and dimension: got %d and %d", maxChunks, gen.Dimension)
	}

	ids := make([]int64, 0, len(messageIDs))
	seen := make(map[int64]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	for page := range slices.Chunk(ids, vector.FilteredCoverageBatchSize) {
		if err := b.readMessageVectorPage(ctx, gen, page, maxChunks, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (b *Backend) readMessageVectorPage(ctx context.Context, gen vector.Generation, ids []int64, maxChunks int, result map[int64][][]float32) error {
	encoded, err := json.Marshal(ids, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode message vector ids: %w", err)
	}
	query := fmt.Sprintf(`
		SELECT e.message_id, e.chunk_index, v.embedding
		  FROM embeddings e
		  JOIN %s v ON v.embedding_id = e.embedding_id
		 WHERE v.generation_id = ?
		   AND e.message_id IN (SELECT value FROM json_each(?))
		   AND e.chunk_index >= 0
		   AND e.chunk_index < ?
		 ORDER BY e.message_id, e.chunk_index`, VectorTableName(gen.Dimension))
	rows, err := b.db.QueryContext(ctx, query, int64(gen.ID), string(encoded), maxChunks)
	if err != nil {
		return fmt.Errorf("load message vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var chunkIndex int
		var blob []byte
		if err := rows.Scan(&id, &chunkIndex, &blob); err != nil {
			return fmt.Errorf("scan message vector: %w", err)
		}
		v, err := blobToFloat32(blob, gen.Dimension)
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
