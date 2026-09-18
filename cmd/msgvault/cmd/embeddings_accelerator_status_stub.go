//go:build !sqlite_vec

package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/vector"
)

func readEmbeddingAccelerator(context.Context, vector.Backend, vector.GenerationID) (embeddingAcceleratorRow, error) {
	return embeddingAcceleratorRow{}, nil
}
