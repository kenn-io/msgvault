package rerank

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/typesafe"
)

const (
	JevModel = typesafe.ModelJev113
	// JevEndpoint labels Docbank's fixed System One route in eval reports.
	JevEndpoint       = "https://api.typesafe.ai/v1/systemone"
	MaxCandidates     = 30
	MaxCandidateBytes = 2048
	jevSecretBinding  = "TYPESAFE_API_KEY" //nolint:gosec // env var name, not a credential
	jevConcurrent     = 8
	// jevCallBudget is the time one provider call gets; a ranking's deadline covers every wave of calls.
	jevCallBudget = 10 * time.Second
)

// ErrInvalidResponse marks scores the caller cannot use.
var ErrInvalidResponse = errors.New("invalid provider response")

type jevScorer interface {
	Rerank(ctx context.Context, request typesafe.RerankRequest) (typesafe.Result, error)
}

// Jev scores candidates through Docbank's TypeSafe client.
type Jev struct{ scorer jevScorer }

type staticSecret string

func (s staticSecret) ResolveSecret(context.Context, string) (string, error) { return string(s), nil }

func jevProfile(shape string) (typesafe.Profile, error) {
	var requestShape typesafe.RequestShape
	switch shape {
	case "per-candidate":
		requestShape = typesafe.RequestShapePerCandidate
	case "batched":
		requestShape = typesafe.RequestShapeBatched
	default:
		return typesafe.Profile{}, fmt.Errorf("unknown Jev request shape %q", shape)
	}
	return typesafe.Profile{
		SecretBinding: jevSecretBinding, RequestShape: requestShape,
		MaxCandidates: MaxCandidates, MaxCandidateBytes: MaxCandidateBytes,
		MaxConcurrentCalls: jevConcurrent,
		RequestTimeout:     time.Duration((MaxCandidates+jevConcurrent-1)/jevConcurrent) * jevCallBudget,
		EgressPolicy: providerhttp.EgressPolicy{
			Scheme: "https", Host: "api.typesafe.ai", Port: 443,
			// api.typesafe.ai resolves to both families, and Docbank rejects any answer outside these.
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")},
		},
	}, nil
}

// NewJev returns a Jev scorer for the per-candidate or batched request shape.
func NewJev(shape, key string) (*Jev, error) {
	profile, err := jevProfile(shape)
	if err != nil {
		return nil, err
	}
	client, err := typesafe.New(profile, staticSecret(key), nil, &http.Client{})
	if err != nil {
		return nil, fmt.Errorf("typesafe client: %w", err)
	}
	return &Jev{scorer: client}, nil
}

// Rerank scores request.Candidates in order. A failed ranking reports no usage.
func (j *Jev) Rerank(ctx context.Context, request Request) (Result, error) {
	result, err := j.scorer.Rerank(ctx, typesafe.RerankRequest{Query: request.Query, Candidates: request.Candidates})
	if err != nil {
		return Result{}, err
	}
	receipt := result.Receipt
	requests := 1
	if receipt.RequestShape == typesafe.RequestShapePerCandidate {
		requests = receipt.CandidateCount
	}
	input, output := receipt.InputTokens, receipt.OutputTokens
	return Result{Scores: result.Scores, Usage: Usage{Requests: requests, InputTokens: &input, OutputTokens: &output, Complete: true}}, nil
}

// SafeFailure reports a known error category without including queries, message
// text, credentials, or provider response bodies.
func SafeFailure(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "provider timeout or cancellation"
	}
	if providerErr, ok := errors.AsType[*typesafe.ProviderError](err); ok && providerErr.StatusCode != 0 {
		return fmt.Sprintf("provider returned HTTP %d", providerErr.StatusCode)
	}
	switch {
	case errors.Is(err, ErrInvalidResponse), errors.Is(err, typesafe.ErrPermanentResponse):
		return "invalid provider response"
	case errors.Is(err, typesafe.ErrCapacityResponse):
		return "request bounds exceeded"
	case errors.Is(err, typesafe.ErrTransientResponse):
		return "provider timeout or transport failure"
	}
	return "provider request failed"
}
