//go:build pgvector

package pgvector

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/vector"
)

// GenerationIDs includes retired generations so erasure cannot leave hidden
// embeddings behind. It does not create, activate or retire any generation.
func (b *Backend) GenerationIDs(ctx context.Context) (ids []vector.GenerationID, retErr error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id FROM index_generations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var id vector.GenerationID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
