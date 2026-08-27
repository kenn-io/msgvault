//go:build !sqlite_vec && !pgvector

package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

func runConfiguredDocumentVectorGeneration(context.Context, *store.Store, int64, int) (vectordocument.ReconcileResult, error) {
	return vectordocument.ReconcileResult{}, errors.New("document vector backend is unavailable: rebuild with sqlite_vec or pgvector support")
}

func runScheduledDocumentVectorGeneration(context.Context, *store.Store, *vectorFeatures, int) error {
	return errors.New("document vector backend is unavailable: rebuild with sqlite_vec or pgvector support")
}
