//go:build pgvector

package pgvector

import (
	"context"

	"go.kenn.io/msgvault/internal/vector"
)

// GenerationIDs includes retired generations so erasure cannot leave hidden
// embeddings behind. It does not create, activate or retire any generation.
func (b *Backend) GenerationIDs(ctx context.Context) ([]vector.GenerationID, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id FROM index_generations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []vector.GenerationID
	for rows.Next() {
		var id vector.GenerationID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
