package visual

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
)

type GenerationID int64
type VectorToken string

type Usage struct {
	TotalTokens int64 `json:"total_tokens"`
	Available   bool  `json:"available"`
}

type EmbeddingResult struct {
	Owner  Owner
	Vector []float32
	Usage  Usage
}

type Provider interface {
	EmbedDocuments(ctx context.Context, documents []DocumentInput) ([]EmbeddingResult, error)
	EmbedQuery(ctx context.Context, query QueryInput) ([]float32, Usage, error)
}

type SearchRequest struct {
	GenerationID GenerationID
	Vector       []float32
	Limit        int
	AfterScore   *float64
	AfterToken   VectorToken
	Person       *personscope.Scope
	SourceID     int64
	MessageID    int64
	Filename     string
	MIMEPrefix   string
	After        *time.Time
	Before       *time.Time
}

type Hit struct {
	Token VectorToken
	Score float64
	Rank  int
}

type Backend interface {
	PutUnpublished(ctx context.Context, token VectorToken, vector []float32) error
	DeleteTokens(ctx context.Context, tokens []VectorToken) error
	Search(ctx context.Context, request SearchRequest) ([]Hit, error)
	LoadOwnerVector(ctx context.Context, generationID GenerationID, owner Owner) ([]float32, error)
}

var (
	ErrProviderBatchTooLarge = errors.New("visual provider batch is too large")
	ErrProviderUnauthorized  = errors.New("visual provider authorization failed")
	ErrProviderRejected      = errors.New("visual provider rejected the input")
	ErrProviderRetryable     = errors.New("visual provider request is retryable")
	ErrProviderMalformed     = errors.New("visual provider returned a malformed response")
)

type ProviderError struct {
	Kind       error
	StatusCode int
	RetryAfter time.Duration
	RetrySet   bool
	Cause      error
}

func (e *ProviderError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("visual provider HTTP %d: %v", e.StatusCode, e.Kind)
	}
	return fmt.Sprintf("visual provider: %v", e.Kind)
}

func (e *ProviderError) Unwrap() []error {
	errors := []error{e.Kind}
	if e.Cause != nil {
		errors = append(errors, e.Cause)
	}
	return errors
}

func providerError(kind error, statusCode int, cause error) error {
	return &ProviderError{
		Kind: kind, StatusCode: statusCode, Cause: cause,
	}
}

func providerRetryError(statusCode int, retryAfter time.Duration, retrySet bool, cause error) error {
	return &ProviderError{
		Kind: ErrProviderRetryable, StatusCode: statusCode,
		RetryAfter: retryAfter, RetrySet: retrySet, Cause: cause,
	}
}

func providerMalformedError(cause error) error {
	if cause == nil {
		cause = ErrProviderRetryable
	} else {
		cause = errors.Join(ErrProviderRetryable, cause)
	}
	return providerError(ErrProviderMalformed, 0, cause)
}

func ProviderRetryAfter(err error) (time.Duration, bool) {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || !providerErr.RetrySet {
		return 0, false
	}
	return providerErr.RetryAfter, true
}
