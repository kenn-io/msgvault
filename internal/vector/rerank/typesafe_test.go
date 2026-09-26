package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func captureResponse(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/typesafe/" + name)
	require.NoError(t, err)
	var capture struct {
		Response json.RawMessage `json:"response"`
	}
	require.NoError(t, json.Unmarshal(data, &capture))
	return capture.Response
}

func TestJevWireCaptures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	per, err := encodeJevCalls("synthetic question", []string{"synthetic candidate"}, "per-candidate")
	require.NoError(err)
	batch, err := encodeJevCalls("synthetic question", []string{"synthetic first", "synthetic second"}, "batched")
	require.NoError(err)
	var perCapture, batchCapture map[string]any
	require.NoError(json.Unmarshal(mustReadCapture(t, "capture_per_candidate.json"), &perCapture))
	require.NoError(json.Unmarshal(mustReadCapture(t, "capture_batched.json"), &batchCapture))
	var got map[string]any
	require.NoError(json.Unmarshal(per[0], &got))
	assert.Equal(perCapture["request"], got)
	require.NoError(json.Unmarshal(batch[0], &got))
	assert.Equal(batchCapture["request"], got)

	perResult, err := decodeJevResponse(captureResponse(t, "capture_per_candidate.json"), []string{"matches"})
	require.NoError(err)
	assert.Equal([]float64{0.15}, perResult.Scores)
	assert.Equal(int64(342), *perResult.Usage.InputTokens)
	assert.True(perResult.Usage.Complete)
	batchResult, err := decodeJevResponse(captureResponse(t, "capture_batched.json"), []string{"candidate_0", "candidate_1"})
	require.NoError(err)
	assert.Equal([]float64{0.37, 0.34}, batchResult.Scores)
}

func mustReadCapture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/typesafe/" + name)
	require.NoError(t, err)
	return data
}

func TestJevBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Log("candidate=2048 query=4096 request=131072 response=65536 max_requests=1000")
	require.NoError(func() error {
		_, err := encodeJevCalls(strings.Repeat("q", 4096), []string{"candidate"}, "batched")
		return err
	}())
	_, err := encodeJevCalls(strings.Repeat("q", 4097), []string{"candidate"}, "batched")
	require.ErrorIs(err, ErrRequestBounds)
	_, err = encodeJevCalls("query", []string{strings.Repeat("x", 2049)}, "batched")
	require.ErrorIs(err, ErrRequestBounds)
	maxCandidates := make([]string, MaxCandidates)
	for i := range maxCandidates {
		maxCandidates[i] = strings.Repeat("x", MaxCandidateBytes)
	}
	requests, err := encodeJevCalls(strings.Repeat("q", typesafeMaxQuery), maxCandidates, "batched")
	require.NoError(err)
	require.Len(requests, 1)
	assert.LessOrEqual(len(requests[0]), typesafeMaxRequest)

	budget := &Budget{MaxRequests: 0, StopUSD: 1}
	scorer, err := NewJev("batched", "secret", budget, nil)
	require.NoError(err)
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: http.NoBody}, nil
	})}
	_, err = scorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"a", "b"}})
	require.ErrorIs(err, ErrRequestLimit)
	assert.Equal(0, budget.attempts, "the request limit is checked before egress")

	preflightBudget := &Budget{MaxRequests: 2, StopUSD: 1, attempts: 1}
	preflightScorer, err := NewJev("per-candidate", "secret", preflightBudget, nil)
	require.NoError(err)
	var preflightCalls atomic.Int32
	preflightScorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		preflightCalls.Add(1)
		return nil, errors.New("unexpected provider call")
	})}
	_, err = preflightScorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"a", "b"}})
	require.ErrorIs(err, ErrRequestLimit)
	assert.Equal(int32(0), preflightCalls.Load(), "remaining request capacity is checked before egress")

	responseBudget := &Budget{MaxRequests: 1000, StopUSD: 1}
	responseScorer, err := NewJev("batched", "secret", responseBudget, nil)
	require.NoError(err)
	responseScorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 65537)))}, nil
	})}
	_, err = responseScorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"a", "b"}})
	require.ErrorIs(err, ErrInvalidResponse)
	assert.Contains(err.Error(), "65536")
}

func TestJevAccounting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	response := captureResponse(t, "capture_batched.json")
	budget := &Budget{MaxRequests: 10, StopUSD: 0.0005, InputUSDPerM: 1, OutputUSDPerM: 2}
	scorer, err := NewJev("batched", "secret", budget, nil)
	require.NoError(err)
	var requests atomic.Int32
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
	})}
	result, err := scorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"a", "b"}})
	require.NoError(err)
	assert.Equal([]float64{0.37, 0.34}, result.Scores)
	assert.Equal(1, result.Usage.Requests)
	assert.Equal(int64(438), *result.Usage.InputTokens)
	assert.Equal(int64(40), *result.Usage.OutputTokens)
	assert.InDelta(0.000518, budget.cost, 1e-9)
	_, err = scorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"a", "b"}})
	require.ErrorIs(err, ErrCostStop)
	assert.Equal(int32(1), requests.Load(), "a measured cost stop prevents another provider call")
}

func TestJevFailureReturnsAttemptedCallsAndPartialUsage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	response := captureResponse(t, "capture_per_candidate.json")
	budget := &Budget{MaxRequests: 10, StopUSD: 1, InputUSDPerM: 1, OutputUSDPerM: 1}
	scorer, err := NewJev("per-candidate", "secret-key", budget, nil)
	require.NoError(err)
	var requests atomic.Int32
	scorer.client = &http.Client{Transport: testTransport(func(_ *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			budget.mu.Lock()
			recorded := budget.cost > 0
			budget.mu.Unlock()
			if recorded {
				break
			}
			time.Sleep(time.Millisecond)
		}
		budget.mu.Lock()
		recorded := budget.cost > 0
		budget.mu.Unlock()
		if !recorded {
			return nil, errors.New("first call usage was not recorded")
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: io.NopCloser(strings.NewReader("private provider body"))}, nil
	})}

	result, err := scorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"first", "second"}})
	require.ErrorContains(err, "HTTP 503")
	assert.Equal(2, result.Usage.Requests)
	assert.Equal(int64(342), *result.Usage.InputTokens)
	assert.Equal(int64(20), *result.Usage.OutputTokens)
	assert.False(result.Usage.Complete)
	assert.Equal("provider returned HTTP 503", SafeFailure(err))
	assert.NotContains(err.Error(), "private provider body")
	assert.NotContains(err.Error(), "secret-key")
}

type contextErrorBody struct{ ctx context.Context }

func (b contextErrorBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (contextErrorBody) Close() error { return nil }

func TestJevBodyReadTimeoutKeepsTimeoutCategory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		scorer, err := NewJev("per-candidate", "secret-key", &Budget{MaxRequests: 1, StopUSD: 1}, nil)
		require.NoError(err)
		scorer.client = &http.Client{Transport: testTransport(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       contextErrorBody{ctx: request.Context()},
			}, nil
		})}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		start := time.Now()
		_, err = scorer.Rerank(ctx, Request{Query: "query", Candidates: []string{"candidate"}})
		require.ErrorIs(err, context.DeadlineExceeded)
		assert.Equal("provider timeout or cancellation", SafeFailure(err))
		assert.Equal(10*time.Second, time.Since(start))
	})
}

func TestJevMissingUsageStopsFurtherCalls(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	response := `{"model":"jev-1.13.0","answers":{"candidate_0":{"type":"noul","noul":0.75}}}`
	budget := &Budget{MaxRequests: 10, StopUSD: 1}
	scorer, err := NewJev("batched", "secret", budget, nil)
	require.NoError(err)
	var requests atomic.Int32
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	result, err := scorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"a"}})
	require.NoError(err)
	assert.Equal([]float64{0.75}, result.Scores)
	assert.Equal(int64(0), *result.Usage.InputTokens)
	assert.Equal(int64(0), *result.Usage.OutputTokens)
	assert.False(result.Usage.Complete)
	_, err = scorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"a"}})
	require.ErrorIs(err, ErrUsageUnknown)
	assert.Equal(int32(1), requests.Load(), "unknown usage prevents another provider call")
}

func TestJevFailureRedactsProviderBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	budget := &Budget{MaxRequests: 10, StopUSD: 1}
	scorer, err := NewJev("batched", "secret", budget, nil)
	require.NoError(err)
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: io.NopCloser(strings.NewReader("candidate secret body"))}, nil
	})}
	_, err = scorer.Rerank(context.Background(), Request{Query: "query", Candidates: []string{"candidate"}})
	require.Error(err)
	assert.NotContains(err.Error(), "candidate secret body")
	assert.NotContains(err.Error(), "secret")
}

func TestSafeFailureCategories(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrRequestLimit, "request limit reached"},
		{ErrCostStop, "local cost stop reached"},
		{ErrUsageUnknown, "provider usage unavailable"},
		{context.DeadlineExceeded, "provider timeout or cancellation"},
		{context.Canceled, "provider timeout or cancellation"},
		{httpStatusError(503), "provider returned HTTP 503"},
		{ErrInvalidResponse, "invalid provider response"},
		{ErrRequestBounds, "request bounds exceeded"},
		{errors.New("request limit reached: secret"), "provider request failed"},
		{errors.New("provider returned HTTP 503 secret"), "provider request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			// Misleading wrapper text must not change classification or reach the report.
			got := SafeFailure(fmt.Errorf("response query exceeds secret: %w", tc.err))
			assert.Equal(t, tc.want, got)
		})
	}
}
