//go:build !pgvector

package pgvector

import (
	"context"

	"go.kenn.io/msgvault/internal/vector/visual"
)

type VisualBackend struct{}

func (b *Backend) Visual() *VisualBackend { return &VisualBackend{} }

func (b *VisualBackend) PutUnpublished(context.Context, visual.VectorToken, []float32) error {
	return ErrNotBuilt
}
func (b *VisualBackend) DeleteTokens(context.Context, []visual.VectorToken) error { return ErrNotBuilt }
func (b *VisualBackend) Search(context.Context, visual.SearchRequest) ([]visual.Hit, error) {
	return nil, ErrNotBuilt
}
func (b *VisualBackend) LoadOwnerVector(context.Context, visual.GenerationID, visual.Owner) ([]float32, error) {
	return nil, ErrNotBuilt
}

var _ visual.Backend = (*VisualBackend)(nil)
