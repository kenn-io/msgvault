//go:build !sqlite_vec && !pgvector

package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

// lowerEmbedWatermarkForRepair is a no-op for builds without a vector backend
// build tag: there is no embeddings store and no watermark to lower. The real
// implementation lives in repair_encoding_vector.go (built with sqlite_vec or
// pgvector). repair-encoding still resets embed_gen on the main DB column,
// which is harmless when vector search is unavailable.
func lowerEmbedWatermarkForRepair(_ context.Context, _ *store.Store, _ int64) error {
	return nil
}
