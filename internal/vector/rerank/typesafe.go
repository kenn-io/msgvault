package rerank

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
)

const (
	JevEndpoint            = "https://api.typesafe.ai/v1/systemone"
	JevModel               = "jev-1.13.0"
	MaxCandidates          = 30
	MaxCandidateBytes      = 2048
	typesafeMaxQuery       = 4096
	typesafeMaxRequest     = 128 << 10
	typesafeMaxResponse    = 64 << 10
	typesafeMaxConcurrent  = 8
	typesafeRequestTimeout = 10 * time.Second
)

// Failure categories let callers report errors without exposing provider content.
var (
	ErrRequestLimit    = errors.New("request limit reached")
	ErrCostStop        = errors.New("local cost stop reached")
	ErrUsageUnknown    = errors.New("provider usage unavailable")
	ErrRequestBounds   = errors.New("request bounds exceeded")
	ErrInvalidResponse = errors.New("invalid provider response")
)

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("provider returned HTTP %d", int(e)) }

// Budget shares request and cost limits across Jev scorers. Set its limits before use.
type Budget struct {
	mu            sync.Mutex
	MaxRequests   int
	StopUSD       float64
	InputUSDPerM  float64
	OutputUSDPerM float64
	attempts      int
	cost          float64
	unknown       bool
	stopped       bool
	failed        bool
}

func (b *Budget) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed {
		return errors.New("rerank provider failed; no further requests will start")
	}
	if b.unknown {
		return ErrUsageUnknown
	}
	if b.stopped || b.cost >= b.StopUSD {
		b.stopped = true
		return ErrCostStop
	}
	if b.attempts >= b.MaxRequests {
		return ErrRequestLimit
	}
	b.attempts++
	return nil
}

func (b *Budget) preflight(requests int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if requests <= 0 || b.attempts+requests > b.MaxRequests {
		return ErrRequestLimit
	}
	if b.failed {
		return errors.New("rerank provider failed; no further requests will start")
	}
	if b.unknown {
		return ErrUsageUnknown
	}
	if b.stopped || b.cost >= b.StopUSD {
		return ErrCostStop
	}
	return nil
}

func (b *Budget) record(usage Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		b.unknown = true
		return
	}
	b.cost += float64(*usage.InputTokens)*b.InputUSDPerM/1e6 +
		float64(*usage.OutputTokens)*b.OutputUSDPerM/1e6
	if b.cost >= b.StopUSD {
		b.stopped = true
	}
}

func (b *Budget) fail() {
	b.mu.Lock()
	b.failed = true
	b.mu.Unlock()
}

// Jev scores message candidates with the TypeSafe API.
type Jev struct {
	shape  string
	key    string
	client *http.Client
	budget *Budget
}

// NewJev creates a scorer. A nil transport uses the default HTTP transport.
func NewJev(shape, key string, budget *Budget, transport http.RoundTripper) (*Jev, error) {
	if shape != "per-candidate" && shape != "batched" {
		return nil, fmt.Errorf("unknown Jev request shape %q", shape)
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("TYPESAFE_API_KEY is required")
	}
	if budget == nil {
		return nil, errors.New("reranker budget is required")
	}
	return &Jev{
		shape: shape,
		key:   key,
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		budget: budget,
	}, nil
}

type jevRequest struct {
	State     any                    `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevQuestion struct {
	Type         string      `json:"type"`
	Instructions string      `json:"instructions"`
	Criteria     jevCriteria `json:"criteria"`
}

type jevCriteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

type jevPerCandidateState struct {
	Query     string `json:"query"`
	Candidate string `json:"candidate"`
}

type jevBatchedState struct {
	Query      string   `json:"query"`
	Candidates []string `json:"candidates"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   *jevUsage            `json:"usage"`
}

type jevAnswer struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

type jevUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

func rankingQuestion(name string) jevQuestion {
	return jevQuestion{
		Type:         "noul",
		Instructions: fmt.Sprintf("Could `%s` be the best answer to `query`?", name),
		Criteria: jevCriteria{
			True:  fmt.Sprintf("The %s contains the specific information needed to answer the query.", name),
			False: fmt.Sprintf("The %s is only topically similar or does not contain the needed evidence.", name),
		},
	}
}

func encodeJevCalls(query string, candidates []string, shape string) ([][]byte, error) {
	if strings.TrimSpace(query) == "" || !utf8.ValidString(query) || len([]byte(query)) > typesafeMaxQuery {
		return nil, fmt.Errorf("%w: query exceeds the 4096-byte Jev limit or is empty", ErrRequestBounds)
	}
	if len(candidates) == 0 || len(candidates) > MaxCandidates {
		return nil, fmt.Errorf("%w: candidate count must be between 1 and %d", ErrRequestBounds, MaxCandidates)
	}
	for i, candidate := range candidates {
		if !utf8.ValidString(candidate) || len([]byte(candidate)) > MaxCandidateBytes {
			return nil, fmt.Errorf("%w: candidate %d exceeds the 2048-byte Jev limit", ErrRequestBounds, i)
		}
	}
	var requests []jevRequest
	switch shape {
	case "per-candidate":
		requests = make([]jevRequest, len(candidates))
		for i, candidate := range candidates {
			requests[i] = jevRequest{
				State:     jevPerCandidateState{Query: query, Candidate: candidate},
				Model:     JevModel,
				Questions: map[string]jevQuestion{"matches": rankingQuestion("candidate")},
			}
		}
	case "batched":
		questions := make(map[string]jevQuestion, len(candidates))
		for i := range candidates {
			name := fmt.Sprintf("candidates[%d]", i)
			questions[fmt.Sprintf("candidate_%d", i)] = rankingQuestion(name)
		}
		requests = []jevRequest{{
			State: jevBatchedState{Query: query, Candidates: slices.Clone(candidates)},
			Model: JevModel, Questions: questions,
		}}
	default:
		return nil, fmt.Errorf("unknown Jev request shape %q", shape)
	}
	out := make([][]byte, len(requests))
	for i, request := range requests {
		body, err := json.Marshal(request)
		if err != nil {
			return nil, errors.New("encode Jev request")
		}
		if len(body) > typesafeMaxRequest {
			return nil, fmt.Errorf("%w: encoded Jev request exceeds 131072 bytes", ErrRequestBounds)
		}
		out[i] = body
	}
	return out, nil
}

func decodeJevResponse(data []byte, questionIDs []string) (Result, error) {
	var response jevResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return Result{}, fmt.Errorf("%w: malformed JSON", ErrInvalidResponse)
	}
	if response.Model != JevModel || len(response.Answers) != len(questionIDs) {
		return Result{}, fmt.Errorf("%w: model or answer count did not match request", ErrInvalidResponse)
	}
	wanted := make(map[string]struct{}, len(questionIDs))
	scores := make([]float64, len(questionIDs))
	for i, id := range questionIDs {
		wanted[id] = struct{}{}
		answer, ok := response.Answers[id]
		if !ok || answer.Type != "noul" || answer.Noul == nil || math.IsNaN(*answer.Noul) ||
			math.IsInf(*answer.Noul, 0) || *answer.Noul < 0 || *answer.Noul > 1 {
			return Result{}, fmt.Errorf("%w: invalid answer", ErrInvalidResponse)
		}
		scores[i] = *answer.Noul
	}
	for id := range response.Answers {
		if _, ok := wanted[id]; !ok {
			return Result{}, fmt.Errorf("%w: unexpected answer", ErrInvalidResponse)
		}
	}
	var usage Usage
	if response.Usage != nil {
		if response.Usage.InputTokens != nil && *response.Usage.InputTokens < 0 ||
			response.Usage.OutputTokens != nil && *response.Usage.OutputTokens < 0 {
			return Result{}, fmt.Errorf("%w: invalid token usage", ErrInvalidResponse)
		}
		usage.InputTokens = validTokenPointer(response.Usage.InputTokens)
		usage.OutputTokens = validTokenPointer(response.Usage.OutputTokens)
	}
	usage.Complete = usage.InputTokens != nil && usage.OutputTokens != nil
	return Result{Scores: scores, Usage: usage}, nil
}

func validTokenPointer(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	usage := *value
	return &usage
}

func (j *Jev) Rerank(ctx context.Context, request Request) (Result, error) {
	calls, err := encodeJevCalls(request.Query, request.Candidates, j.shape)
	if err != nil {
		return emptyJevResult(), err
	}
	if err := j.budget.preflight(len(calls)); err != nil {
		return emptyJevResult(), err
	}
	scores := make([]float64, len(request.Candidates))
	var usageMu sync.Mutex
	attempted := 0
	var totalInput, totalOutput int64
	usageComplete := true
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(typesafeMaxConcurrent)
	for callIndex, body := range calls {
		group.Go(func() error {
			if err := j.budget.reserve(); err != nil {
				return err
			}
			usageMu.Lock()
			attempted++
			usageMu.Unlock()
			ids := []string{"matches"}
			if j.shape == "batched" {
				ids = make([]string, len(request.Candidates))
				for i := range request.Candidates {
					ids[i] = fmt.Sprintf("candidate_%d", i)
				}
			}
			result, callErr := j.send(groupCtx, body, ids)
			if callErr != nil {
				usageMu.Lock()
				usageComplete = false
				usageMu.Unlock()
				j.budget.fail()
				return callErr
			}
			usageMu.Lock()
			defer usageMu.Unlock()
			if j.shape == "batched" {
				copy(scores, result.Scores)
			} else {
				scores[callIndex] = result.Scores[0]
			}
			if result.Usage.InputTokens == nil {
				usageComplete = false
			} else {
				totalInput += *result.Usage.InputTokens
			}
			if result.Usage.OutputTokens == nil {
				usageComplete = false
			} else {
				totalOutput += *result.Usage.OutputTokens
			}
			j.budget.record(result.Usage)
			return nil
		})
	}
	groupErr := group.Wait()
	result := Result{Scores: scores, Usage: Usage{
		Requests: attempted, InputTokens: &totalInput, OutputTokens: &totalOutput, Complete: usageComplete,
	}}
	if groupErr != nil {
		return result, fmt.Errorf("rerank requests failed: %w", groupErr)
	}
	return result, nil
}

func emptyJevResult() Result {
	input, output := int64(0), int64(0)
	return Result{Usage: Usage{InputTokens: &input, OutputTokens: &output, Complete: true}}
}

// SafeFailure reports a known error category without including queries, message
// text, credentials, or provider response bodies.
func SafeFailure(err error) string {
	for _, category := range []error{ErrRequestLimit, ErrCostStop, ErrUsageUnknown, ErrRequestBounds, ErrInvalidResponse} {
		if errors.Is(err, category) {
			return category.Error()
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "provider timeout or cancellation"
	}
	if status, ok := errors.AsType[httpStatusError](err); ok {
		return status.Error()
	}
	return "provider request failed"
}

func (j *Jev) send(ctx context.Context, body []byte, ids []string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, typesafeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, JevEndpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, errors.New("construct Jev request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.key)
	response, err := j.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, errors.New("provider request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Result{}, httpStatusError(response.StatusCode)
	}
	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return Result{}, fmt.Errorf("%w: non-JSON content type", ErrInvalidResponse)
	}
	limited := io.LimitReader(response.Body, typesafeMaxResponse+1)
	body, err = io.ReadAll(limited)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("%w: cannot read body", ErrInvalidResponse)
	}
	if len(body) > typesafeMaxResponse {
		return Result{}, fmt.Errorf("%w: response exceeds 65536 bytes", ErrInvalidResponse)
	}
	return decodeJevResponse(body, ids)
}
