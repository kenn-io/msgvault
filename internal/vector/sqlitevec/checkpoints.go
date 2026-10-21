//go:build sqlite_vec

package sqlitevec

import (
	"go.kenn.io/msgvault/internal/vector"
)

func (b *Backend) EmbeddingCheckpoints() vector.EmbeddingCheckpoints {
	return vector.EmbeddingCheckpoints{DB: b.db, Rebind: func(query string) string { return query }}
}
