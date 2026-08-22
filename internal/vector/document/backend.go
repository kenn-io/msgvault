package document

import (
	"context"
	"errors"
)

// ErrInvalidVector reports an invalid document-vector request or a vector
// that cannot participate in cosine similarity.
var ErrInvalidVector = errors.New("invalid document vector")

// Embedding pairs an opaque publication token with its vector.
type Embedding struct {
	Token  string
	Vector []float32
}

// Hit is one cosine-similarity result. Higher scores are better and Rank is
// one-based after deterministic score/token ordering.
type Hit struct {
	Token string
	Score float64
	Rank  int
}

// Backend stores unpublished vectors by opaque publication token. Publication
// authority remains in the main store; this interface deliberately knows
// nothing about archive or document identities encoded by callers. A canceled
// PutUnpublished must expose neither a partial batch nor a partially replaced
// token so claim-heartbeat loss can safely abort publication.
type Backend interface {
	PutUnpublished(ctx context.Context, generationID GenerationID, dimension int, embeddings []Embedding) error
	DeleteTokens(ctx context.Context, generationID GenerationID, tokens []string) error
	Search(ctx context.Context, generationID GenerationID, dimension int, query []float32, k int) ([]Hit, error)
}
