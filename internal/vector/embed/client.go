// Package embed implements an OpenAI-compatible /v1/embeddings HTTP client.
// It is used by the vector search pipeline to convert email text into
// embedding vectors suitable for ANN search.
package embed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/msgvault/internal/vector"
)

// ErrPermanent4xx marks a non-retryable HTTP 4xx response from the
// embeddings endpoint. Use errors.Is(err, ErrPermanent4xx) to detect
// it. Kit reports the status and failure reason without echoing provider
// response bodies. HTTP 408, 429 and 5xx remain retryable.
var ErrPermanent4xx = vector.ErrPermanent4xx

// Config controls an embeddings Client. The zero value is not usable; callers
// must set Endpoint, Model, and Dimension at a minimum.
type Config struct {
	// Endpoint is the base URL including /v1 (e.g. "http://host:8080/v1").
	// The request path "/embeddings" is appended.
	Endpoint string
	// APIKey is an optional bearer token sent as Authorization: Bearer <key>.
	APIKey string
	// Model is the model name passed in the request body.
	Model string
	// DocumentPrefix is prepended to every document chunk before it is sent.
	DocumentPrefix string
	// QueryPrefix is prepended to every query before it is sent.
	QueryPrefix string
	// Dimension is the expected vector dimension. Responses whose vectors
	// differ are rejected with an error.
	Dimension int
	// Timeout is the per-request HTTP timeout. Defaults to 30s when zero.
	Timeout time.Duration
	// MaxRetries is the maximum number of HTTP attempts for a single Embed
	// call. Defaults to 3 when zero. Kit retries HTTP 408, 429, 5xx and
	// transport failures with jittered exponential backoff.
	MaxRetries int
	// BeforeRequest reauthorizes each concrete HTTP attempt. A returned error
	// is propagated without retrying. Nil leaves the client ungated.
	BeforeRequest BeforeRequestFunc
	// RejectRedirects prevents provider responses from replaying embedding input
	// to another URL. BeforeRequest clients always reject redirects as well.
	RejectRedirects bool
}

// Client calls an OpenAI-compatible /v1/embeddings endpoint.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient constructs a Client, applying defaults for Timeout and MaxRetries.
func NewClient(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	return &Client{cfg: cfg, http: newHTTPClient(cfg.Timeout, nil, cfg.RejectRedirects || cfg.BeforeRequest != nil)}
}

// Embed embeds document chunks in input order. Kit owns transport validation
// and retry policy; MaxRetries is the total attempt budget. Empty input is a no-op.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	return c.embed(ctx, embedconfig.RoleDocument, inputs)
}

func (c *Client) embed(ctx context.Context, role embedconfig.Role, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	httpClient := *c.http
	if c.cfg.BeforeRequest != nil {
		// Consent failure ends this call, including Kit retries. Each concrete
		// attempt still checks authorization at the transport boundary.
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
		base := httpClient.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		httpClient.Transport = beforeRequestTransport{base: base, before: func(ctx context.Context) error {
			err := c.cfg.BeforeRequest(ctx)
			if err != nil {
				cancel(err)
			}
			return err
		}}
	}
	client, err := embedclient.New(embedclient.Options{
		Model: embedconfig.Model{Name: c.cfg.Model, Dimensions: c.cfg.Dimension,
			Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
		Roles:      embedconfig.Roles{DocumentPrefix: c.cfg.DocumentPrefix, QueryPrefix: c.cfg.QueryPrefix},
		Deployment: embedconfig.Deployment{BaseURL: c.cfg.Endpoint, TrustPrivateNetwork: true},
		// The workers already pack batches and preserve document boundaries.
		Batch:     embedconfig.Batch{Items: math.MaxInt},
		Transport: embedconfig.Transport{Timeout: c.cfg.Timeout},
		APIKey:    c.cfg.APIKey, HTTP: &httpClient,
		Retry: embedclient.Retry{
			MaxAttempts:    c.cfg.MaxRetries,
			InitialBackoff: 200 * time.Millisecond,
			MaxBackoff:     25600 * time.Millisecond,
			MaxRetryAfter:  time.Hour,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	vectors, err := client.EmbedTexts(ctx, role, inputs)
	if err == nil {
		return vectors, nil
	}
	if cause := beforeRequestCause(err); cause != nil {
		return nil, cause
	}
	if apiErr, ok := errors.AsType[*embedclient.APIError](err); ok {
		if apiErr.StatusCode >= 300 && apiErr.StatusCode < 400 {
			return nil, ErrEmbeddingProviderRedirect
		}
		if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && !apiErr.Retryable() {
			return nil, fmt.Errorf("%w: %w", ErrPermanent4xx, err)
		}
		return nil, fmt.Errorf("embed: %w", err)
	}
	if errors.Is(err, embedclient.ErrInvalidVector) {
		return nil, fmt.Errorf("%w: %w", vector.ErrInvalidProviderVector, err)
	}
	if _, ok := errors.AsType[*embedclient.TransportError](err); ok {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	return nil, fmt.Errorf("%w: %w", vector.ErrInvalidProviderShape, err)
}

// EmbedQuery embeds one query and returns its single vector.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.embed(ctx, embedconfig.RoleQuery, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("%w: embed query expected exactly one vector, got %d", vector.ErrInvalidProviderShape, len(vecs))
	}
	return vecs[0], nil
}

// EmbedDocuments flattens document chunks for the OpenAI-compatible request,
// then restores the original document boundaries without changing chunk order.
func (c *Client) EmbedDocuments(ctx context.Context, documents []DocumentInput) ([][][]float32, error) {
	var inputs []string
	for _, document := range documents {
		inputs = append(inputs, document.Chunks...)
	}

	vecs, err := c.embed(ctx, embedconfig.RoleDocument, inputs)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(inputs) {
		return nil, fmt.Errorf("%w: embed documents expected %d vectors, got %d", vector.ErrInvalidProviderShape, len(inputs), len(vecs))
	}

	documentVecs := make([][][]float32, len(documents))
	offset := 0
	for i, document := range documents {
		next := offset + len(document.Chunks)
		documentVecs[i] = vecs[offset:next]
		offset = next
	}
	return documentVecs, nil
}
