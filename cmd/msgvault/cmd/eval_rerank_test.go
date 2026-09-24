//go:build fts5 && sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/rerank"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
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
	per, err := encodeJevCalls("synthetic question", []string{"synthetic candidate"}, "per-candidate")
	require.NoError(t, err)
	batch, err := encodeJevCalls("synthetic question", []string{"synthetic first", "synthetic second"}, "batched")
	require.NoError(t, err)
	var perCapture, batchCapture map[string]any
	require.NoError(t, json.Unmarshal(mustReadCapture(t, "capture_per_candidate.json"), &perCapture))
	require.NoError(t, json.Unmarshal(mustReadCapture(t, "capture_batched.json"), &batchCapture))
	var got map[string]any
	require.NoError(t, json.Unmarshal(per[0], &got))
	assert.Equal(perCapture["request"], got)
	require.NoError(t, json.Unmarshal(batch[0], &got))
	assert.Equal(batchCapture["request"], got)

	perResult, err := decodeJevResponse(captureResponse(t, "capture_per_candidate.json"), []string{"matches"})
	require.NoError(t, err)
	assert.Equal([]float64{0.15}, perResult.Scores)
	assert.Equal(int64(342), *perResult.Usage.InputTokens)
	assert.True(perResult.Usage.Complete)
	batchResult, err := decodeJevResponse(captureResponse(t, "capture_batched.json"), []string{"candidate_0", "candidate_1"})
	require.NoError(t, err)
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
	t.Log("candidate=2048 query=4096 request=131072 response=65536 max_requests=1000")
	require.NoError(t, func() error {
		_, err := encodeJevCalls(strings.Repeat("q", 4096), []string{"candidate"}, "batched")
		return err
	}())
	_, err := encodeJevCalls(strings.Repeat("q", 4097), []string{"candidate"}, "batched")
	require.Error(t, err)
	_, err = encodeJevCalls("query", []string{strings.Repeat("x", 2049)}, "batched")
	require.Error(t, err)
	maxCandidates := make([]string, typesafeMaxCandidates)
	for i := range maxCandidates {
		maxCandidates[i] = strings.Repeat("x", typesafeMaxCandidate)
	}
	requests, err := encodeJevCalls(strings.Repeat("q", typesafeMaxQuery), maxCandidates, "batched")
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.LessOrEqual(len(requests[0]), typesafeMaxRequest)

	budget := &rerankBudget{maxRequests: 0, stopUSD: 1}
	scorer, err := newJevReranker("batched", "secret", budget)
	require.NoError(t, err)
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: http.NoBody}, nil
	})}
	_, err = scorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"a", "b"}})
	require.Error(t, err)
	assert.Equal(0, budget.attempts, "the request limit is checked before egress")

	preflightBudget := &rerankBudget{maxRequests: 2, stopUSD: 1, attempts: 1}
	preflightScorer, err := newJevReranker("per-candidate", "secret", preflightBudget)
	require.NoError(t, err)
	var preflightCalls atomic.Int32
	preflightScorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		preflightCalls.Add(1)
		return nil, errors.New("unexpected provider call")
	})}
	_, err = preflightScorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"a", "b"}})
	require.Error(t, err)
	assert.Equal(int32(0), preflightCalls.Load(), "remaining request capacity is checked before egress")

	responseBudget := &rerankBudget{maxRequests: 1000, stopUSD: 1}
	responseScorer, err := newJevReranker("batched", "secret", responseBudget)
	require.NoError(t, err)
	responseScorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: ioNopCloser{strings.NewReader(strings.Repeat("x", 65537))}}, nil
	})}
	_, err = responseScorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"a", "b"}})
	require.Error(t, err)
	assert.Contains(err.Error(), "65536")
}

func TestJevAccounting(t *testing.T) {
	assert := assert.New(t)
	response := captureResponse(t, "capture_batched.json")
	budget := &rerankBudget{maxRequests: 10, stopUSD: 0.0005, inputUSDPerM: 1, outputUSDPerM: 2}
	scorer, err := newJevReranker("batched", "secret", budget)
	require.NoError(t, err)
	var requests atomic.Int32
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: ioNopCloser{strings.NewReader(string(response))}}, nil
	})}
	result, err := scorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"a", "b"}})
	require.NoError(t, err)
	assert.Equal([]float64{0.37, 0.34}, result.Scores)
	assert.Equal(1, result.Usage.Requests)
	assert.Equal(int64(438), *result.Usage.InputTokens)
	assert.Equal(int64(40), *result.Usage.OutputTokens)
	assert.InDelta(0.000518, budget.cost, 1e-9)
	_, err = scorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"a", "b"}})
	require.ErrorContains(t, err, "local cost stop reached")
	assert.Equal(int32(1), requests.Load(), "a measured cost stop prevents another provider call")
}

func TestJevFailureReturnsAttemptedCallsAndPartialUsage(t *testing.T) {
	response := captureResponse(t, "capture_per_candidate.json")
	budget := &rerankBudget{maxRequests: 10, stopUSD: 1, inputUSDPerM: 1, outputUSDPerM: 1}
	scorer, err := newJevReranker("per-candidate", "secret-key", budget)
	require.NoError(t, err)
	var requests atomic.Int32
	scorer.client = &http.Client{Transport: testTransport(func(_ *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: ioNopCloser{strings.NewReader(string(response))}}, nil
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
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: ioNopCloser{strings.NewReader("private provider body")}}, nil
	})}

	result, err := scorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"first", "second"}})
	require.ErrorContains(t, err, "HTTP 503")
	assert.Equal(t, 2, result.Usage.Requests)
	assert.Equal(t, int64(342), *result.Usage.InputTokens)
	assert.Equal(t, int64(20), *result.Usage.OutputTokens)
	assert.False(t, result.Usage.Complete)
	assert.Equal(t, "provider returned HTTP 503", safeRerankFailure(err))
	assert.NotContains(t, err.Error(), "private provider body")
	assert.NotContains(t, err.Error(), "secret-key")
}

type contextErrorBody struct{ ctx context.Context }

func (b contextErrorBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (contextErrorBody) Close() error { return nil }

func TestJevBodyReadTimeoutKeepsTimeoutCategory(t *testing.T) {
	scorer, err := newJevReranker("per-candidate", "secret-key", &rerankBudget{maxRequests: 1, stopUSD: 1})
	require.NoError(t, err)
	scorer.client = &http.Client{Transport: testTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       contextErrorBody{ctx: request.Context()},
		}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = scorer.Rerank(ctx, rerank.Request{Query: "query", Candidates: []string{"candidate"}})
	require.Error(t, err)
	assert.Equal(t, "provider timeout or cancellation", safeRerankFailure(err))
}

func TestJevMissingUsageStopsFurtherCalls(t *testing.T) {
	assert := assert.New(t)
	response := `{"model":"jev-1.13.0","answers":{"candidate_0":{"type":"noul","noul":0.75}}}`
	budget := &rerankBudget{maxRequests: 10, stopUSD: 1}
	scorer, err := newJevReranker("batched", "secret", budget)
	require.NoError(t, err)
	var requests atomic.Int32
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: ioNopCloser{strings.NewReader(response)}}, nil
	})}
	result, err := scorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"a"}})
	require.NoError(t, err)
	assert.Equal([]float64{0.75}, result.Scores)
	assert.Equal(int64(0), *result.Usage.InputTokens)
	assert.Equal(int64(0), *result.Usage.OutputTokens)
	assert.False(result.Usage.Complete)
	_, err = scorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"a"}})
	require.ErrorContains(t, err, "usage is unknown")
	assert.Equal(int32(1), requests.Load(), "unknown usage prevents another provider call")
}

func TestJevFailureRedactsProviderBody(t *testing.T) {
	assert := assert.New(t)
	budget := &rerankBudget{maxRequests: 10, stopUSD: 1}
	scorer, err := newJevReranker("batched", "secret", budget)
	require.NoError(t, err)
	scorer.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: ioNopCloser{strings.NewReader("candidate secret body")}}, nil
	})}
	_, err = scorer.Rerank(context.Background(), rerank.Request{Query: "query", Candidates: []string{"candidate"}})
	require.Error(t, err)
	assert.NotContains(err.Error(), "candidate secret body")
	assert.NotContains(err.Error(), "secret")
}

type evalRerankRecorder struct {
	requests     map[string][]rerank.Request
	deadlines    []time.Time
	failAt       map[string]int
	failUsage    rerank.Usage
	promoteText  string
	delay        time.Duration
	factoryCalls int
}

type recordingReranker struct {
	shape    string
	recorder *evalRerankRecorder
}

func (r *evalRerankRecorder) makeReranker(shape, _ string, _ *rerankBudget) (rerank.Reranker, error) {
	r.factoryCalls++
	if r.requests == nil {
		r.requests = make(map[string][]rerank.Request)
	}
	return &recordingReranker{shape: shape, recorder: r}, nil
}

func (r *recordingReranker) Rerank(ctx context.Context, request rerank.Request) (rerank.Result, error) {
	request.Candidates = append([]string(nil), request.Candidates...)
	r.recorder.requests[r.shape] = append(r.recorder.requests[r.shape], request)
	if deadline, ok := ctx.Deadline(); ok {
		r.recorder.deadlines = append(r.recorder.deadlines, deadline)
	}
	if r.recorder.delay > 0 {
		time.Sleep(r.recorder.delay)
	}
	if r.recorder.failAt[r.shape] == len(r.recorder.requests[r.shape]) {
		return rerank.Result{Usage: r.recorder.failUsage}, errors.New("fake provider failure")
	}
	scores := make([]float64, len(request.Candidates))
	for i := range scores {
		if r.recorder.promoteText != "" {
			if strings.Contains(strings.ToLower(request.Candidates[i]), r.recorder.promoteText) {
				scores[i] = 1
			}
			continue
		}
		scores[i] = float64(len(scores)-i) / float64(len(scores))
	}
	input, output := int64(5), int64(2)
	requests := 1
	if r.shape == "per-candidate" {
		requests = len(request.Candidates)
	}
	return rerank.Result{Scores: scores, Usage: rerank.Usage{
		Requests: requests, InputTokens: &input, OutputTokens: &output, Complete: true,
	}}, nil
}

func preserveEvalRerankGlobals(t *testing.T) {
	t.Helper()
	oldCfg := cfg
	oldQrels, oldTopics, oldModes, oldDocKey := evalQrels, evalTopics, evalModes, evalDocKey
	oldLimit, oldJSON := evalLimit, evalJSON
	oldJev, oldTop, oldMaxRequests := evalRerankJev, evalRerankTop, evalRerankMaxRequests
	oldCost, oldInput, oldOutput := evalRerankCostStopUSD, evalRerankInputUSDPerM, evalRerankOutputUSDPerM
	t.Cleanup(func() {
		cfg = oldCfg
		evalQrels, evalTopics, evalModes, evalDocKey = oldQrels, oldTopics, oldModes, oldDocKey
		evalLimit, evalJSON = oldLimit, oldJSON
		evalRerankJev, evalRerankTop, evalRerankMaxRequests = oldJev, oldTop, oldMaxRequests
		evalRerankCostStopUSD, evalRerankInputUSDPerM, evalRerankOutputUSDPerM = oldCost, oldInput, oldOutput
	})
}

func newEvalRerankTestCommand(t *testing.T, out *bytes.Buffer, inputPrice, outputPrice bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().Float64("rerank-input-usd-per-million", 0, "")
	cmd.Flags().Float64("rerank-output-usd-per-million", 0, "")
	if inputPrice {
		require.NoError(t, cmd.Flags().Set("rerank-input-usd-per-million", "1"))
	}
	if outputPrice {
		require.NoError(t, cmd.Flags().Set("rerank-output-usd-per-million", "1"))
	}
	cmd.SetContext(t.Context())
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	return cmd
}

func prepareEvalRerankRun(t *testing.T, shapes string, topicCount int) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	preserveEvalRerankGlobals(t)
	dir := t.TempDir()
	seedRankingDivergenceArchiveIn(t, dir)
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	evalModes, evalDocKey, evalLimit, evalJSON = "fts", "message", 10, true
	evalRerankJev, evalRerankTop, evalRerankMaxRequests = shapes, 2, 1000
	evalRerankCostStopUSD, evalRerankInputUSDPerM, evalRerankOutputUSDPerM = 5, 1, 1
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	var topics, qrels strings.Builder
	for i := range topicCount {
		qid := fmt.Sprintf("q%d", i+1)
		fmt.Fprintf(&topics, "%s\trenewal\n", qid)
		fmt.Fprintf(&qrels, "%s 0 <m1@example.com> 1\n", qid)
	}
	evalQrels = writeEvalFile(t, dir, "qrels.txt", qrels.String())
	evalTopics = writeEvalFile(t, dir, "topics.tsv", topics.String())
	out := &bytes.Buffer{}
	return newEvalRerankTestCommand(t, out, true, true), out
}

func TestRunEvalReranksFTSCandidatesWithIndependentDeadlines(t *testing.T) {
	assert := assert.New(t)
	cmd, out := prepareEvalRerankRun(t, "batched,per-candidate", 2)
	recorder := &evalRerankRecorder{delay: 20 * time.Millisecond}
	require.NoError(t, runEvalWithRerankerFactory(cmd, nil, recorder.makeReranker))
	require.Len(t, recorder.deadlines, 4)
	for i := 0; i < len(recorder.deadlines); i += 2 {
		assert.GreaterOrEqual(recorder.deadlines[i+1].Sub(recorder.deadlines[i]), recorder.delay/2)
	}
	for _, shape := range []string{"batched", "per-candidate"} {
		require.Len(t, recorder.requests[shape], 2)
		for _, request := range recorder.requests[shape] {
			assert.Equal("renewal", request.Query)
			require.Len(t, request.Candidates, 2)
			candidateText := strings.ToLower(strings.Join(request.Candidates, "\n"))
			assert.Contains(candidateText, "lease renewal terms")
			assert.Contains(candidateText, "signed and returned")
			assert.NotContains(candidateText, "<m1@example.com>")
			assert.NotContains(candidateText, "me@example.com")
		}
	}
	var report struct {
		Rerank struct {
			Complete            bool    `json:"complete"`
			MaxRequests         int     `json:"max_requests"`
			CostStopUSD         float64 `json:"cost_stop_usd"`
			InputUSDPerMillion  float64 `json:"input_usd_per_million"`
			OutputUSDPerMillion float64 `json:"output_usd_per_million"`
			Results             map[string]map[string]struct {
				Status               string  `json:"status"`
				Requests             int     `json:"requests"`
				Topics               int     `json:"topics"`
				RequestsPerQuery     float64 `json:"requests_per_query"`
				InputTokensPerQuery  float64 `json:"input_tokens_per_query"`
				OutputTokensPerQuery float64 `json:"output_tokens_per_query"`
				CostPerQueryUSD      float64 `json:"cost_per_query_usd"`
				Latency              struct {
					P95MS *float64 `json:"p95_ms"`
				} `json:"latency"`
			} `json:"results"`
		} `json:"rerank_results"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.True(report.Rerank.Complete)
	assert.Equal(1000, report.Rerank.MaxRequests)
	assert.InDelta(5.0, report.Rerank.CostStopUSD, 1e-9)
	assert.InDelta(1.0, report.Rerank.InputUSDPerMillion, 1e-9)
	assert.InDelta(1.0, report.Rerank.OutputUSDPerMillion, 1e-9)
	for shape, requests := range map[string]int{"batched": 2, "per-candidate": 4} {
		arm := report.Rerank.Results["fts"][shape]
		assert.Equal("complete", arm.Status)
		assert.Equal(2, arm.Topics)
		assert.Equal(requests, arm.Requests)
		assert.InDelta(float64(requests)/2, arm.RequestsPerQuery, 1e-9)
		assert.InDelta(5.0, arm.InputTokensPerQuery, 1e-9)
		assert.InDelta(2.0, arm.OutputTokensPerQuery, 1e-9)
		assert.InDelta(0.000007, arm.CostPerQueryUSD, 1e-12)
		assert.NotNil(arm.Latency.P95MS)
	}
}

type ioNopCloser struct{ *strings.Reader }

func (ioNopCloser) Close() error { return nil }

type fakeReranker struct{}

func (fakeReranker) Rerank(_ context.Context, _ rerank.Request) (rerank.Result, error) {
	return rerank.Result{Scores: []float64{0.1, 0.9}, Usage: rerank.Usage{Complete: true}}, nil
}

func TestEvalRerankShortlist(t *testing.T) {
	assert := assert.New(t)
	keys, result, err := rerankEvalKeys(context.Background(), fakeReranker{}, "query",
		[]string{"first", "second", "tail"}, []string{"one", "two"})
	require.NoError(t, err)
	assert.Equal([]string{"second", "first", "tail"}, keys)
	assert.Equal([]float64{0.1, 0.9}, result.Scores)
}

func TestRunEvalRerankFailureKeepsCompleteBaseline(t *testing.T) {
	cmd, out := prepareEvalRerankRun(t, "batched,per-candidate", 3)
	input, output := int64(5), int64(2)
	recorder := &evalRerankRecorder{
		failAt:    map[string]int{"batched": 2},
		failUsage: rerank.Usage{Requests: 1, InputTokens: &input, OutputTokens: &output},
	}
	err := runEvalWithRerankerFactory(cmd, nil, recorder.makeReranker)
	require.ErrorContains(t, err, "provider request failed")
	require.Len(t, recorder.requests["batched"], 2, "provider work stops after the failed request")
	require.Len(t, recorder.requests["per-candidate"], 1)

	var report struct {
		TopicsEvaluated int `json:"topics_evaluated"`
		Results         map[string]struct {
			Topics int `json:"topics"`
		} `json:"results"`
		Rerank struct {
			Complete bool                                             `json:"complete"`
			Results  map[string]map[string]map[string]json.RawMessage `json:"results"`
		} `json:"rerank_results"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Equal(t, 3, report.TopicsEvaluated)
	assert.Equal(t, 3, report.Results["fts"].Topics)
	assert.False(t, report.Rerank.Complete)
	arm := report.Rerank.Results["fts"]["batched"]
	var status string
	require.NoError(t, json.Unmarshal(arm["status"], &status))
	assert.Equal(t, "failed", status)
	assert.NotContains(t, arm, "Hit@1")
	var requests int
	require.NoError(t, json.Unmarshal(arm["requests"], &requests))
	assert.Equal(t, 2, requests)
	var inputTokens int64
	require.NoError(t, json.Unmarshal(arm["input_tokens"], &inputTokens))
	assert.Equal(t, int64(10), inputTokens)
	var outputTokens int64
	require.NoError(t, json.Unmarshal(arm["output_tokens"], &outputTokens))
	assert.Equal(t, int64(4), outputTokens)
	assert.Contains(t, string(arm["cost_usd"]), "null")
	var incompleteStatus string
	var incompleteTopics int
	incomplete := report.Rerank.Results["fts"]["per-candidate"]
	require.NoError(t, json.Unmarshal(incomplete["status"], &incompleteStatus))
	require.NoError(t, json.Unmarshal(incomplete["topics"], &incompleteTopics))
	assert.Equal(t, "incomplete", incompleteStatus)
	assert.Equal(t, 1, incompleteTopics)
	assert.NotContains(t, incomplete, "Hit@1")
	assert.NotContains(t, incomplete, "latency")
}

func TestRunEvalRerankQualityMetricsFollowProviderScoresAcrossModes(t *testing.T) {
	cmd, out := prepareEvalRerankRun(t, "batched", 1)
	dataDir := cfg.Data.DataDir
	evalQrels = writeEvalFile(t, dataDir, "quality-qrels.txt", "q1 0 <m2@example.com> 1\n")
	evalModes = "fts,vector,hybrid"
	c := evalVectorConfig(t, vector.APIFormatOpenAI, "test-model")
	c.Data.DataDir = dataDir
	c.Vector.Embeddings.Dimension = 3
	_, endpoint := embedTestServer(t, `{"data":[{"index":0,"embedding":[1,0,0]}]}`)
	c.Vector.Embeddings.Endpoint = endpoint
	cfg = c
	s, err := store.Open(c.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchema())
	seedEmbeddedGeneration(t, dataDir, c.DatabaseDSN(), s, c.Vector, 1, 2)
	require.NoError(t, sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(context.Background(), sqlitevec.Options{
		Path: filepath.Join(dataDir, "vectors.db"), MainPath: c.DatabaseDSN(),
		Dimension: 3, MainDB: s.DB(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	generation, err := backend.ActiveGeneration(context.Background())
	require.NoError(t, err)
	require.NoError(t, backend.Upsert(context.Background(), generation.ID, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0}, SourceCharLen: 32},
		{MessageID: 2, Vector: []float32{0, 1, 0}, SourceCharLen: 32},
	}))

	recorder := &evalRerankRecorder{promoteText: "weekly digest"}
	require.NoError(t, runEvalWithRerankerFactory(cmd, nil, recorder.makeReranker))
	var report struct {
		Results      map[string]json.RawMessage `json:"results"`
		RerankResult struct {
			Results map[string]map[string]map[string]json.RawMessage `json:"results"`
		} `json:"rerank_results"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	for _, mode := range []string{"fts", "vector", "hybrid"} {
		var baseline struct {
			Hit1 float64 `json:"Hit@1"`
		}
		require.NoError(t, json.Unmarshal(report.Results[mode], &baseline))
		var reranked struct {
			Hit1   float64 `json:"Hit@1"`
			Status string  `json:"status"`
		}
		require.NoError(t, json.Unmarshal(report.RerankResult.Results[mode]["batched"]["Hit@1"], &reranked.Hit1))
		require.NoError(t, json.Unmarshal(report.RerankResult.Results[mode]["batched"]["status"], &reranked.Status))
		assert.InDelta(t, 0, baseline.Hit1, 1e-9, mode)
		assert.InDelta(t, 1, reranked.Hit1, 1e-9, mode)
		assert.Equal(t, "complete", reranked.Status, mode)
	}
	assert.Len(t, recorder.requests["batched"], 3)
}

func TestSafeRerankFailureCategories(t *testing.T) {
	cases := []struct {
		message string
		want    string
	}{
		{"rerank request limit reached before provider call", "request limit reached"},
		{"rerank local cost stop reached; no further requests will start", "local cost stop reached"},
		{"rerank usage is unknown; no further requests will start", "provider usage unavailable"},
		{"provider request timed out or was canceled", "provider timeout or cancellation"},
		{"provider returned HTTP 503", "provider returned HTTP 503"},
		{"provider returned HTTP 503 secret", "provider request failed"},
		{"invalid Jev response", "invalid provider response"},
		{"candidate 1 exceeds the 2048-byte Jev limit", "request bounds exceeded"},
		{"secret provider exploded", "provider request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := safeRerankFailure(errors.New(tc.message))
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "secret")
		})
	}
	wrapped := fmt.Errorf("rerank requests failed: %w", errors.New("provider returned HTTP 503"))
	assert.Equal(t, "provider returned HTTP 503", safeRerankFailure(wrapped))
}

func TestValidateJevRequestEstimate(t *testing.T) {
	shapes := []string{"per-candidate", "batched"}
	require.NoError(t, validateJevRequestEstimate(10, 3, shapes, 30, 1000))
	require.ErrorContains(t, validateJevRequestEstimate(11, 3, shapes, 30, 1000), "conservative request estimate")
}

func TestRunEvalPreflightsJevRequestEstimateBeforeOpeningArchive(t *testing.T) {
	preserveEvalRerankGlobals(t)
	dir := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	evalModes, evalDocKey, evalLimit, evalJSON = "fts,vector,hybrid", "message", 100, true
	evalRerankJev, evalRerankTop, evalRerankMaxRequests = "per-candidate,batched", 30, 1000
	evalRerankCostStopUSD, evalRerankInputUSDPerM, evalRerankOutputUSDPerM = 5, 1, 1
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	var topics, qrels strings.Builder
	for i := range 11 {
		qid := fmt.Sprintf("q%d", i+1)
		fmt.Fprintf(&topics, "%s\trenewal\n", qid)
		fmt.Fprintf(&qrels, "%s 0 <m1@example.com> 1\n", qid)
	}
	evalQrels = writeEvalFile(t, dir, "qrels.txt", qrels.String())
	evalTopics = writeEvalFile(t, dir, "topics.tsv", topics.String())
	cmd := newEvalRerankTestCommand(t, &bytes.Buffer{}, true, true)
	recorder := &evalRerankRecorder{}
	err := runEvalWithRerankerFactory(cmd, nil, recorder.makeReranker)
	require.ErrorContains(t, err, "conservative request estimate")
	assert.Zero(t, recorder.factoryCalls)
	_, statErr := os.Stat(filepath.Join(dir, "msgvault.db"))
	assert.True(t, os.IsNotExist(statErr), "the rejected estimate must not open the archive")
}

func TestEvalRerankCandidateText(t *testing.T) {
	assert := assert.New(t)
	text := truncateUTF8Bytes(strings.Repeat("界", 1000), typesafeMaxCandidate)
	assert.LessOrEqual(len([]byte(text)), 2048)
	assert.True(utf8.ValidString(text))
	assert.Equal("abc", truncateUTF8Bytes("abc", 2048))
}

func TestEvalRerankOptIn(t *testing.T) {
	assert := assert.New(t)
	old := evalRerankJev
	t.Cleanup(func() { evalRerankJev = old })
	evalRerankJev = ""
	options, err := readEvalRerankOptions(nil)
	require.NoError(t, err)
	assert.Empty(options.Shapes)
}

func TestReadEvalRerankOptionsRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name        string
		inputPrice  bool
		outputPrice bool
		key         string
		wantError   string
		mutate      func()
	}{
		{name: "conversation key", inputPrice: true, outputPrice: true, key: "test-key", wantError: "--rerank-jev requires --doc-key=message", mutate: func() { evalDocKey = "conversation" }},
		{name: "top outside provider bound", inputPrice: true, outputPrice: true, key: "test-key", wantError: "--rerank-top must be between", mutate: func() { evalRerankTop = typesafeMaxCandidates + 1 }},
		{name: "top exceeds retrieval depth", inputPrice: true, outputPrice: true, key: "test-key", wantError: "cannot exceed --limit", mutate: func() { evalRerankTop = evalLimit + 1 }},
		{name: "nonpositive request limit", inputPrice: true, outputPrice: true, key: "test-key", wantError: "--rerank-max-requests must be positive", mutate: func() { evalRerankMaxRequests = 0 }},
		{name: "invalid cost stop", inputPrice: true, outputPrice: true, key: "test-key", wantError: "must be a positive finite number", mutate: func() { evalRerankCostStopUSD = math.NaN() }},
		{name: "missing input price", outputPrice: true, key: "test-key", wantError: "--rerank-input-usd-per-million is required"},
		{name: "missing output price", inputPrice: true, key: "test-key", wantError: "--rerank-output-usd-per-million is required"},
		{name: "negative input price", inputPrice: true, outputPrice: true, key: "test-key", wantError: "must be a finite nonnegative number", mutate: func() { evalRerankInputUSDPerM = -1 }},
		{name: "nonfinite output price", inputPrice: true, outputPrice: true, key: "test-key", wantError: "must be a finite nonnegative number", mutate: func() { evalRerankOutputUSDPerM = math.Inf(1) }},
		{name: "unknown shape", inputPrice: true, outputPrice: true, key: "test-key", wantError: "invalid --rerank-jev value", mutate: func() { evalRerankJev = "unknown" }},
		{name: "empty shapes", inputPrice: true, outputPrice: true, key: "test-key", wantError: "must name per-candidate or batched", mutate: func() { evalRerankJev = ",," }},
		{name: "missing API key", inputPrice: true, outputPrice: true, key: " ", wantError: "TYPESAFE_API_KEY is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preserveEvalRerankGlobals(t)
			cfg = config.NewDefaultConfig()
			evalDocKey, evalLimit = "message", 10
			evalRerankJev, evalRerankTop, evalRerankMaxRequests = "batched", 2, 100
			evalRerankCostStopUSD, evalRerankInputUSDPerM, evalRerankOutputUSDPerM = 1, 1, 1
			t.Setenv("TYPESAFE_API_KEY", tc.key)
			cmd := newEvalRerankTestCommand(t, &bytes.Buffer{}, tc.inputPrice, tc.outputPrice)
			if tc.mutate != nil {
				tc.mutate()
			}
			_, err := readEvalRerankOptions(cmd)
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

func TestPrepareEvalCandidatesRequiresMessageIdentity(t *testing.T) {
	_, err := prepareEvalCandidates(t.Context(), nil, []string{"<message@example.com>"}, nil, embed.PreprocessConfig{}, 1)
	require.ErrorContains(t, err, "has no message identity")
}

func TestHitDepthLabel(t *testing.T) {
	assert := assert.New(t)
	report := evalReport{cutoffs: eval.CutoffsForDepth(5), diag: &runDiagnostics{}}
	var output bytes.Buffer
	require.NoError(t, report.table(&output))
	assert.Contains(output.String(), "Hit@5")
}

type evalRerankFailingWriter struct{ err error }

func (w evalRerankFailingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestEvalReportTablesReturnWriterErrors(t *testing.T) {
	writeErr := errors.New("write failed")
	writer := evalRerankFailingWriter{err: writeErr}

	require.ErrorIs(t, (evalReport{}).table(writer), writeErr)
	require.ErrorIs(t, newEvalRerankReport(evalRerankOptions{}).table(writer, eval.StandardCutoffs), writeErr)
}

func TestEvalRerankReport(t *testing.T) {
	assert := assert.New(t)
	inputPrice, outputPrice := 1.0, 2.0
	report := newEvalRerankReport(evalRerankOptions{
		Shapes: []string{"batched"}, Top: 30, MaxRequests: 1000,
		CostStopUSD: 2, InputUSDPerM: inputPrice, OutputUSDPerM: outputPrice,
	})
	arm := report.arm("hybrid", "batched")
	arm.addUsage(rerank.Usage{Requests: 1}, inputPrice, outputPrice)
	arm.addQuality([]string{"relevant", "other"}, map[string]struct{}{"relevant": {}}, eval.CutoffsForDepth(5), time.Millisecond)
	var output bytes.Buffer
	err := evalReport{modes: []string{"hybrid"}, aggs: map[string]*eval.Aggregate{"hybrid": {}},
		lats:    map[string]*eval.LatencyTracker{"hybrid": &eval.LatencyTracker{}},
		catAggs: map[string]map[string]*eval.Aggregate{}, cutoffs: eval.CutoffsForDepth(5),
		diag: &runDiagnostics{}, rerank: report}.json(&output)
	require.NoError(t, err)
	assert.Contains(output.String(), `"usage_complete": false`)
	assert.Contains(output.String(), `"input_tokens": 0`)
	assert.Contains(output.String(), `"output_tokens": 0`)
	assert.Contains(output.String(), `"cost_usd": null`)
	assert.Contains(output.String(), `"input_tokens_per_query": 0`)
	assert.Contains(output.String(), `"output_tokens_per_query": 0`)
	assert.Contains(output.String(), `"cost_per_query_usd": null`)
	assert.Contains(output.String(), `"Hit@5"`)
}

func TestEvalRerankFailure(t *testing.T) {
	report := newEvalRerankReport(evalRerankOptions{
		Shapes: []string{"batched"}, Top: 30, MaxRequests: 1000,
		CostStopUSD: 2, InputUSDPerM: 1, OutputUSDPerM: 2,
	})
	arm := report.arm("hybrid", "batched")
	input, outputTokens := int64(5), int64(2)
	arm.addUsage(rerank.Usage{Requests: 1, InputTokens: &input, OutputTokens: &outputTokens}, 1, 2)
	arm.addQuality([]string{"relevant", "other"}, map[string]struct{}{"relevant": {}}, eval.CutoffsForDepth(5), time.Millisecond)
	arm.Status = "failed"
	arm.Complete = false
	arm.Error = "provider request failed"
	report.Complete = false
	report.Failure = "provider request failed"

	base := &eval.Aggregate{}
	base.Add(eval.Evaluate([]string{"relevant"}, map[string]struct{}{"relevant": {}}, eval.CutoffsForDepth(5)))
	var output bytes.Buffer
	err := (evalReport{
		modes: []string{"hybrid"}, aggs: map[string]*eval.Aggregate{"hybrid": base},
		lats:    map[string]*eval.LatencyTracker{"hybrid": &eval.LatencyTracker{}},
		catAggs: map[string]map[string]*eval.Aggregate{}, cutoffs: eval.CutoffsForDepth(5),
		diag: &runDiagnostics{}, rerank: report,
	}).json(&output)
	require.NoError(t, err)
	assert.Contains(t, output.String(), `"Hit@5"`)
	assert.Contains(t, output.String(), `"status": "failed"`)
	assert.Contains(t, output.String(), `"failure": "provider request failed"`)
	assert.Contains(t, output.String(), `"usage_complete": false`)
	assert.Contains(t, output.String(), `"input_tokens": 5`)
	assert.Contains(t, output.String(), `"output_tokens": 2`)
	assert.Contains(t, output.String(), `"cost_usd": null`)
	var document map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(output.Bytes(), &document))
	var rerankJSON struct {
		Results map[string]map[string]map[string]json.RawMessage `json:"results"`
	}
	require.NoError(t, json.Unmarshal(document["rerank_results"], &rerankJSON))
	assert.NotContains(t, rerankJSON.Results["hybrid"]["batched"], "Hit@1")
	assert.NotContains(t, rerankJSON.Results["hybrid"]["batched"], "Hit@5")

	var table bytes.Buffer
	require.NoError(t, report.table(&table, eval.CutoffsForDepth(5)))
	var failedRow string
	for line := range strings.SplitSeq(table.String(), "\n") {
		if strings.Contains(line, "hybrid") && strings.Contains(line, "batched") && strings.Contains(line, "failed") {
			failedRow = line
			break
		}
	}
	fields := strings.Fields(failedRow)
	require.GreaterOrEqual(t, len(fields), 16)
	assert.Equal(t, []string{"hybrid", "batched", "failed", "false", "1", "-", "-", "-", "-"}, fields[:9])
	assert.Equal(t, []string{"1", "-", "5", "-", "2", "-", "unknown"}, fields[9:16])
}

func TestEvalJSONOutputIsDeterministic(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, printJSONTo(&output, map[string]any{
		"z": 1,
		"a": map[string]int{"y": 2, "b": 1},
	}))
	text := output.String()
	assert.Less(t, strings.Index(text, `"a"`), strings.Index(text, `"z"`))
	_, inner, found := strings.Cut(text, `"a"`)
	require.True(t, found)
	assert.Less(t, strings.Index(inner, `"b"`), strings.Index(inner, `"y"`))
}
