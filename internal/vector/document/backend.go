package document

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector"
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
	}, json.Deterministic(true))
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

const (
	vectorBatchLimit = 1000
	vectorTokenLimit = 1024
)

// ReadHitPage reads up to k+1 ordered (token, score, distance) rows and closes
// them; the extra row decides Exhausted and the next cursor.
func ReadHitPage(rows *sql.Rows, k, afterRank int) (HitPage, error) {
	defer func() { _ = rows.Close() }()
	hits := make([]Hit, 0, k+1)
	distances := make([]float64, 0, k+1)
	for rows.Next() {
		var hit Hit
		var distance float64
		if err := rows.Scan(&hit.Token, &hit.Score, &distance); err != nil {
			return HitPage{}, fmt.Errorf("scan document vector hit: %w", err)
		}
		hit.Rank = afterRank + len(hits) + 1
		hits = append(hits, hit)
		distances = append(distances, distance)
	}
	if err := rows.Err(); err != nil {
		return HitPage{}, fmt.Errorf("iterate document vector hits: %w", err)
	}
	page := HitPage{Exhausted: len(hits) <= k}
	if len(hits) > k {
		page.Hits = hits[:k]
	} else {
		page.Hits = hits
	}
	if !page.Exhausted {
		var err error
		page.NextCursor, err = EncodePageCursor(distances[k-1], hits[k-1].Token, hits[k-1].Rank)
		if err != nil {
			return HitPage{}, err
		}
	}
	return page, nil
}

// ValidatePut checks a PutUnpublished batch before a backend touches storage.
func ValidatePut(generationID GenerationID, dimension int, embeddings []Embedding) error {
	if generationID <= 0 || dimension <= 0 || len(embeddings) > vectorBatchLimit {
		return fmt.Errorf("%w: generation, dimension, or batch bound", ErrInvalidVector)
	}
	seen := make(map[string]struct{}, len(embeddings))
	for i, embedding := range embeddings {
		if err := validateToken(embedding.Token); err != nil {
			return fmt.Errorf("embedding %d: %w", i, err)
		}
		if _, ok := seen[embedding.Token]; ok {
			return fmt.Errorf("%w: duplicate token %q", ErrInvalidVector, embedding.Token)
		}
		seen[embedding.Token] = struct{}{}
		if len(embedding.Vector) != dimension {
			return fmt.Errorf("%w: token %q has %d dimensions, want %d",
				vector.ErrDimensionMismatch, embedding.Token, len(embedding.Vector), dimension)
		}
		if err := validateVector(embedding.Vector); err != nil {
			return fmt.Errorf("token %q: %w", embedding.Token, err)
		}
	}
	return nil
}

// ValidateTokens checks a DeleteTokens batch.
func ValidateTokens(generationID GenerationID, tokens []string) error {
	if generationID <= 0 || len(tokens) > vectorBatchLimit {
		return fmt.Errorf("%w: generation or token batch bound", ErrInvalidVector)
	}
	for _, token := range tokens {
		if err := validateToken(token); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSearch checks a Search or SearchPage request.
func ValidateSearch(generationID GenerationID, dimension int, query []float32, k int) error {
	if generationID <= 0 || dimension <= 0 || k <= 0 || k > vectorBatchLimit {
		return fmt.Errorf("%w: generation, dimension, or result bound", ErrInvalidVector)
	}
	if len(query) != dimension {
		return fmt.Errorf("%w: query has %d dimensions, want %d", vector.ErrDimensionMismatch, len(query), dimension)
	}
	return validateVector(query)
}

func validateToken(token string) error {
	if token == "" || len(token) > vectorTokenLimit ||
		!utf8.ValidString(token) || strings.ContainsRune(token, 0) {
		return fmt.Errorf("%w: token must contain 1..%d bytes", ErrInvalidVector, vectorTokenLimit)
	}
	return nil
}

func validateVector(values []float32) error {
	var norm float64
	for _, value := range values {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%w: non-finite component", ErrInvalidVector)
		}
		norm += f * f
	}
	if norm == 0 {
		return fmt.Errorf("%w: zero-norm vector", ErrInvalidVector)
	}
	return nil
}
