//go:build pgvector

package pgvector

import (
	"go.kenn.io/msgvault/internal/sqldialect"
	"go.kenn.io/msgvault/internal/vector"
)

func (b *Backend) EmbeddingCheckpoints() vector.EmbeddingCheckpoints {
	return vector.EmbeddingCheckpoints{DB: b.db, Rebind: sqldialect.RebindPostgreSQL, LockGeneration: true}
}
