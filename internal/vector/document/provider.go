package document

import (
	"context"

	"go.kenn.io/msgvault/internal/vector"
)

// Provider is the document-building subset of embed.SemanticClient.
// Query embedding belongs to retrieval, not generation publication.
type Provider interface {
	EmbedDocuments(ctx context.Context, documents []vector.DocumentInput) ([][][]float32, error)
}
