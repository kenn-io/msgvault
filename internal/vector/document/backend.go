package document

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
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

// HitPage is one deterministic global vector page. Exhausted is authoritative.
type HitPage struct {
	Hits       []Hit
	NextCursor string
	Exhausted  bool
}

const pageCursorVersion = 1

type pageCursor struct {
	Version  int     `json:"v"`
	Distance float64 `json:"d"`
	Token    string  `json:"t"`
	Rank     int     `json:"r"`
}

// EncodePageCursor records the last globally ordered vector hit. Backends use
// it to continue after that hit even when earlier obsolete rows are deleted.
func EncodePageCursor(distance float64, token string, rank int) (string, error) {
	if math.IsNaN(distance) || math.IsInf(distance, 0) || token == "" || !utf8.ValidString(token) || rank < 1 {
		return "", fmt.Errorf("%w: invalid document vector cursor", ErrInvalidVector)
	}
	payload, err := json.Marshal(pageCursor{
		Version: pageCursorVersion, Distance: distance, Token: token, Rank: rank,
	})
	if err != nil {
		return "", fmt.Errorf("encode document vector cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// DecodePageCursor returns the last distance, token, and stable rank carried
// by an opaque backend cursor. An empty cursor starts at the first hit.
func DecodePageCursor(cursor string) (distance float64, token string, rank int, err error) {
	if cursor == "" {
		return 0, "", 0, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", 0, fmt.Errorf("%w: invalid document vector cursor", ErrInvalidVector)
	}
	var decoded pageCursor
	if err := json.Unmarshal(payload, &decoded); err != nil ||
		decoded.Version != pageCursorVersion || math.IsNaN(decoded.Distance) ||
		math.IsInf(decoded.Distance, 0) || decoded.Token == "" ||
		!utf8.ValidString(decoded.Token) || decoded.Rank < 1 {
		return 0, "", 0, fmt.Errorf("%w: invalid document vector cursor", ErrInvalidVector)
	}
	return decoded.Distance, decoded.Token, decoded.Rank, nil
}

// PagedBackend supports scope-complete semantic retrieval without treating a
// global top-k cutoff as the final scoped candidate bound.
type PagedBackend interface {
	SearchPage(ctx context.Context, generationID GenerationID, dimension int, query []float32, cursor string, limit int) (HitPage, error)
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
