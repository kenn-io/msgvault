//go:build !sqlite_vec

package sqlitevec

import (
	"context"

	"go.kenn.io/msgvault/internal/vector/document"
)

// DocumentBackend is unavailable without sqlite_vec support.
type DocumentBackend struct{}

var _ document.Backend = (*DocumentBackend)(nil)

func (b *Backend) DocumentBackend() *DocumentBackend { return &DocumentBackend{} }

func (b *DocumentBackend) PutUnpublished(context.Context, document.GenerationID, int, []document.Embedding) error {
	return ErrNotBuilt
}

func (b *DocumentBackend) DeleteTokens(context.Context, document.GenerationID, []string) error {
	return ErrNotBuilt
}

func (b *DocumentBackend) Search(context.Context, document.GenerationID, int, []float32, int) ([]document.Hit, error) {
	return nil, ErrNotBuilt
}
