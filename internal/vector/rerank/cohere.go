package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxCandidates is the largest candidate list one request may carry.
	// Cohere bills a query with up to 100 documents as one search unit, and
	// OpenRouter enforces the same ceiling for every rerank model it lists.
	MaxCandidates = 100

	// maxCohereResponseBytes bounds the provider reply. OpenRouter echoes
	// each document back in its result, so the reply is roughly the size of
	// the request plus scores.
	maxCohereResponseBytes = 8 << 20
	// maxProviderMessageBytes bounds the provider's own error text that is
	// kept for the daemon log.
	maxProviderMessageBytes = 200
)

// Failure categories. They carry no query, candidate, or credential text, so
// API and MCP responses may report them to the caller.
const (
	FailureTimeout         = "timeout"
	FailureProviderStatus  = "provider_status"
	FailureInvalidResponse = "invalid_response"
	FailureTransport       = "transport"
	FailureRequest         = "invalid_request"
)

// Error is a reranking failure. Category is safe to return to a caller;
// Error() may also include the provider's own error message, which belongs
// in the daemon log only.
type Error struct {
	Category string
	Status   int
	message  string
}

func (e *Error) Error() string {
	if e.message == "" {
		return "rerank: " + e.Category
	}
	return "rerank: " + e.Category + ": " + e.message
}

// FailureCategory returns the caller-safe category of a reranking error.
func FailureCategory(err error) string {
	if rerankErr, ok := errors.AsType[*Error](err); ok {
		return rerankErr.Category
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	return FailureTransport
}

// CohereOptions configures a client for the Cohere-style rerank contract:
// POST {endpoint}/rerank with a model, a query, and a document list, answered
// by one relevance score per document index. OpenRouter serves every rerank
// model it lists (Cohere, Voyage, Qwen, and others) through this contract, and
// Cohere, Jina, and vLLM serve the same shape.
type CohereOptions struct {
	// Endpoint is the provider API root, such as https://openrouter.ai/api/v1.
	Endpoint string
	// APIKey is sent as a bearer token when non-empty.
	APIKey string
	Model  string
	// Timeout bounds one request. Zero leaves the caller's context in charge.
	Timeout time.Duration
	// HTTPClient overrides the transport. Redirects are refused either way,
	// so the credential is never replayed to another origin.
	HTTPClient *http.Client
}

// CohereClient scores candidates through a Cohere-style /rerank endpoint.
type CohereClient struct {
	url     string
	apiKey  string
	model   string
	timeout time.Duration
	client  *http.Client
}

var _ Reranker = (*CohereClient)(nil)

// NewCohereClient validates options and returns a client.
func NewCohereClient(opts CohereOptions) (*CohereClient, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(opts.Endpoint), "/")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("rerank endpoint must be an http or https URL with a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("rerank endpoint must not contain credentials, a query, or a fragment")
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return nil, errors.New("rerank model is required")
	}
	base := http.Client{}
	if opts.HTTPClient != nil {
		base = *opts.HTTPClient
	}
	base.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &CohereClient{
		url:     endpoint + "/rerank",
		apiKey:  opts.APIKey,
		model:   model,
		timeout: opts.Timeout,
		client:  &base,
	}, nil
}

// Model returns the configured model identifier.
func (c *CohereClient) Model() string { return c.model }

type cohereRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type cohereResponse struct {
	Results []struct {
		Index          *int     `json:"index"`
		RelevanceScore *float64 `json:"relevance_score"`
	} `json:"results"`
	Usage *struct {
		TotalTokens *int64 `json:"total_tokens"`
	} `json:"usage"`
}

type cohereErrorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// Rerank scores every candidate in one request. It returns the scores in the
// request's candidate order and never a partial result.
func (c *CohereClient) Rerank(ctx context.Context, request Request) (Result, error) {
	if strings.TrimSpace(request.Query) == "" {
		return Result{}, &Error{Category: FailureRequest, message: "empty query"}
	}
	if len(request.Candidates) == 0 {
		return Result{Scores: []float64{}, Usage: Usage{Complete: true}}, nil
	}
	if len(request.Candidates) > MaxCandidates {
		return Result{}, &Error{Category: FailureRequest,
			message: fmt.Sprintf("%d candidates exceed the limit of %d", len(request.Candidates), MaxCandidates)}
	}
	body, err := json.Marshal(cohereRequest{
		Model:     c.model,
		Query:     request.Query,
		Documents: request.Candidates,
		TopN:      len(request.Candidates),
	})
	if err != nil {
		return Result{}, &Error{Category: FailureRequest, message: "encode request"}
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, &Error{Category: FailureRequest, message: "construct request"}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	usage := Usage{Requests: 1}
	response, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Result{Usage: usage}, &Error{Category: FailureTimeout, message: "provider did not answer in time"}
		}
		return Result{Usage: usage}, &Error{Category: FailureTransport, message: "provider request failed"}
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxCohereResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Result{Usage: usage}, &Error{Category: FailureTimeout, message: "provider response did not finish in time"}
		}
		return Result{Usage: usage}, &Error{Category: FailureTransport, message: "read provider response"}
	}
	if response.StatusCode != http.StatusOK {
		return Result{Usage: usage}, &Error{
			Category: FailureProviderStatus,
			Status:   response.StatusCode,
			message:  fmt.Sprintf("HTTP %d%s", response.StatusCode, providerMessage(data)),
		}
	}
	if len(data) > maxCohereResponseBytes {
		return Result{Usage: usage}, &Error{Category: FailureInvalidResponse, message: "provider response is too large"}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return Result{Usage: usage}, &Error{Category: FailureInvalidResponse, message: "provider returned a non-JSON response"}
	}
	return decodeCohereResponse(data, len(request.Candidates), usage)
}

func decodeCohereResponse(data []byte, candidates int, usage Usage) (Result, error) {
	var decoded cohereResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Result{Usage: usage}, &Error{Category: FailureInvalidResponse, message: "decode provider response"}
	}
	if decoded.Usage != nil && decoded.Usage.TotalTokens != nil && *decoded.Usage.TotalTokens >= 0 {
		tokens := *decoded.Usage.TotalTokens
		usage.InputTokens = &tokens
		usage.Complete = true
	}
	if len(decoded.Results) != candidates {
		return Result{Usage: usage}, &Error{Category: FailureInvalidResponse,
			message: fmt.Sprintf("provider scored %d of %d candidates", len(decoded.Results), candidates)}
	}
	scores := make([]float64, candidates)
	seen := make([]bool, candidates)
	for _, result := range decoded.Results {
		if result.Index == nil || result.RelevanceScore == nil {
			return Result{Usage: usage}, &Error{Category: FailureInvalidResponse, message: "result lacks an index or score"}
		}
		index, score := *result.Index, *result.RelevanceScore
		if index < 0 || index >= candidates || seen[index] {
			return Result{Usage: usage}, &Error{Category: FailureInvalidResponse, message: "result index is out of range or repeated"}
		}
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return Result{Usage: usage}, &Error{Category: FailureInvalidResponse, message: "result score is not in [0,1]"}
		}
		seen[index] = true
		scores[index] = score
	}
	return Result{Scores: scores, Usage: usage}, nil
}

// providerMessage extracts a short provider error message for the daemon
// log. Providers put it under error.message (OpenRouter, vLLM) or message
// (Cohere).
func providerMessage(data []byte) string {
	var decoded cohereErrorResponse
	if json.Unmarshal(data, &decoded) != nil {
		return ""
	}
	message := strings.TrimSpace(decoded.Error.Message)
	if message == "" {
		message = strings.TrimSpace(decoded.Message)
	}
	if message == "" {
		return ""
	}
	if len(message) > maxProviderMessageBytes {
		message = message[:maxProviderMessageBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return ": " + message
}
