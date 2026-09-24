//go:build sqlite_vec

package cmd

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
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/rerank"
	"golang.org/x/sync/errgroup"
)

const (
	typesafeEndpoint       = "https://api.typesafe.ai/v1/systemone"
	typesafeModel          = "jev-1.13.0"
	typesafeMaxCandidates  = 30
	typesafeMaxCandidate   = 2048
	typesafeMaxQuery       = 4096
	typesafeMaxRequest     = 128 << 10
	typesafeMaxResponse    = 64 << 10
	typesafeMaxConcurrent  = 8
	typesafeRequestTimeout = 10 * time.Second
	typesafeRunTimeout     = 30 * time.Minute
)

var (
	evalRerankJev           string
	evalRerankTop           int
	evalRerankMaxRequests   int
	evalRerankCostStopUSD   float64
	evalRerankInputUSDPerM  float64
	evalRerankOutputUSDPerM float64
)

type evalRerankOptions struct {
	Shapes        []string
	Top           int
	MaxRequests   int
	CostStopUSD   float64
	InputUSDPerM  float64
	OutputUSDPerM float64
	APIKey        string
	Preprocess    embed.PreprocessConfig
}

type evalRerankerFactory func(string, string, *rerankBudget) (rerank.Reranker, error)

type evalRerankArm struct {
	Agg           *eval.Aggregate
	Lat           *eval.LatencyTracker
	Requests      int
	InputTokens   *int64
	OutputTokens  *int64
	CostUSD       *float64
	UsageComplete bool
	Complete      bool
	Status        string
	Error         string
}

type evalRerankReport struct {
	Shapes        []string
	Top           int
	MaxRequests   int
	CostStopUSD   float64
	InputUSDPerM  float64
	OutputUSDPerM float64
	Model         string
	Endpoint      string
	Preprocess    embed.PreprocessConfig
	Complete      bool
	Failure       string
	Results       map[string]map[string]*evalRerankArm
}

func newEvalRerankReport(options evalRerankOptions) *evalRerankReport {
	return &evalRerankReport{
		Shapes: slices.Clone(options.Shapes), Top: options.Top,
		MaxRequests: options.MaxRequests, CostStopUSD: options.CostStopUSD,
		InputUSDPerM: options.InputUSDPerM, OutputUSDPerM: options.OutputUSDPerM,
		Model: typesafeModel, Endpoint: typesafeEndpoint, Preprocess: options.Preprocess,
		Complete: true, Results: make(map[string]map[string]*evalRerankArm),
	}
}

func (r *evalRerankReport) arm(mode, shape string) *evalRerankArm {
	if r.Results[mode] == nil {
		r.Results[mode] = make(map[string]*evalRerankArm)
	}
	if r.Results[mode][shape] == nil {
		input, output := int64(0), int64(0)
		r.Results[mode][shape] = &evalRerankArm{
			Agg: &eval.Aggregate{}, Lat: &eval.LatencyTracker{}, Complete: true, Status: "complete",
			InputTokens: &input, OutputTokens: &output, CostUSD: new(0.0), UsageComplete: true,
		}
	}
	return r.Results[mode][shape]
}

func (a *evalRerankArm) addUsage(usage rerank.Usage, inputPrice, outputPrice float64) {
	a.Requests += usage.Requests
	if usage.InputTokens != nil && a.InputTokens != nil {
		*a.InputTokens += *usage.InputTokens
	}
	if usage.OutputTokens != nil && a.OutputTokens != nil {
		*a.OutputTokens += *usage.OutputTokens
	}
	if !usage.Complete || usage.InputTokens == nil || usage.OutputTokens == nil {
		a.UsageComplete = false
		a.CostUSD = nil
		return
	}
	if a.CostUSD != nil {
		*a.CostUSD += float64(*usage.InputTokens)*inputPrice/1e6 + float64(*usage.OutputTokens)*outputPrice/1e6
	}
}

func (a *evalRerankArm) addQuality(ranked []string, rel map[string]struct{}, cutoffs eval.Cutoffs, elapsed time.Duration) {
	a.Agg.Add(eval.Evaluate(ranked, rel, cutoffs))
	a.Lat.Add(elapsed)
}

func hitColumns(cutoffs eval.Cutoffs) (string, string) {
	return "Hit@1", fmt.Sprintf("Hit@%d", min(10, eval.HitDepth(cutoffs)))
}

func (r *evalRerankReport) table(w io.Writer, cutoffs eval.Cutoffs) error {
	if _, err := fmt.Fprintln(w, "\nJev reranking"); err != nil {
		return fmt.Errorf("write rerank report: %w", err)
	}
	if _, err := fmt.Fprintf(w, "  shapes\t%s\n  top\t%d\n  request limit\t%d\n  local cost stop\t$%.6f\n  input price\t$%.6f / million tokens\n  output price\t$%.6f / million tokens\n",
		strings.Join(r.Shapes, ","), r.Top, r.MaxRequests, r.CostStopUSD, r.InputUSDPerM, r.OutputUSDPerM); err != nil {
		return fmt.Errorf("write rerank report: %w", err)
	}
	_, hit10 := hitColumns(cutoffs)
	writer := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(writer, "MODE\tSHAPE\tstatus\tusage complete\ttopics\tHit@1\t%s\tnDCG@%d\tp95 ms\trequests\trequests/q\tinput tokens\tinput/q\toutput tokens\toutput/q\tcost/query\n", hit10, cutoffs.NDCG); err != nil {
		return fmt.Errorf("write rerank report: %w", err)
	}
	for _, mode := range sortedRerankModes(r.Results) {
		for _, shape := range r.Shapes {
			arm := r.Results[mode][shape]
			if arm == nil {
				if _, err := fmt.Fprintf(writer, "%s\t%s\tunrun\t-\t-\t-\t-\t-\t-\t0\t-\t0\t-\t0\t-\t-\n", mode, shape); err != nil {
					return fmt.Errorf("write rerank report: %w", err)
				}
				continue
			}
			if arm.Agg == nil || arm.Agg.N == 0 || !arm.Complete || arm.Status != "complete" {
				topics := 0
				if arm.Agg != nil {
					topics = arm.Agg.N
				}
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%t\t%d\t-\t-\t-\t-\t%d\t-\t%s\t-\t%s\t-\t%s\n", mode, shape, arm.Status, arm.UsageComplete, topics, arm.Requests,
					formatOptionalTokenCount(arm.InputTokens), formatOptionalTokenCount(arm.OutputTokens), formatCostPerQuery(arm.CostUSD, topics)); err != nil {
					return fmt.Errorf("write rerank report: %w", err)
				}
				continue
			}
			s := arm.Agg.Mean()
			l := arm.Lat.Summary()
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%t\t%d\t%.3f\t%.3f\t%.3f\t%.1f\t%d\t%.1f\t%s\t%s\t%s\t%s\t%s\n",
				mode, shape, arm.Status, arm.UsageComplete, arm.Agg.N, s.Hit1, s.Hit10, s.NDCG, l.P95MS,
				arm.Requests, float64(arm.Requests)/float64(arm.Agg.N), formatOptionalTokenCount(arm.InputTokens),
				formatOptionalAverage(arm.InputTokens, arm.Agg.N), formatOptionalTokenCount(arm.OutputTokens),
				formatOptionalAverage(arm.OutputTokens, arm.Agg.N), formatCostPerQuery(arm.CostUSD, arm.Agg.N)); err != nil {
				return fmt.Errorf("write rerank report: %w", err)
			}
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("write rerank report: %w", err)
	}
	if r.Failure != "" {
		if _, err := fmt.Fprintf(w, "  status\tincomplete: %s\n", r.Failure); err != nil {
			return fmt.Errorf("write rerank report: %w", err)
		}
	}
	return nil
}

func sortedRerankModes(results map[string]map[string]*evalRerankArm) []string {
	modes := make([]string, 0, len(results))
	for mode := range results {
		modes = append(modes, mode)
	}
	slices.Sort(modes)
	return modes
}

func formatOptionalAverage(value *int64, queries int) string {
	if value == nil {
		return "unknown"
	}
	if queries <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.1f", float64(*value)/float64(queries))
}

func formatOptionalTokenCount(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}

func formatCostPerQuery(value *float64, queries int) string {
	if value == nil {
		return "unknown"
	}
	if queries <= 0 {
		return "0"
	}
	return fmt.Sprintf("$%.6f", *value/float64(queries))
}

func (r *evalRerankReport) json(cutoffs eval.Cutoffs) map[string]any {
	hit1, hit10 := hitColumns(cutoffs)
	metrics := func(arm *evalRerankArm) map[string]any {
		out := map[string]any{
			"status": arm.Status, "requests": arm.Requests,
			"usage_complete": arm.UsageComplete,
			"input_tokens":   arm.InputTokens, "output_tokens": arm.OutputTokens,
			"cost_usd": arm.CostUSD,
		}
		if arm.Error != "" {
			out["error"] = arm.Error
		}
		if arm.Agg != nil {
			out["topics"] = arm.Agg.N
		}
		if arm.Agg == nil || !arm.Complete || arm.Status != "complete" || arm.Agg.N == 0 {
			return out
		}
		s := arm.Agg.Mean()
		out["requests_per_query"] = float64(arm.Requests) / float64(arm.Agg.N)
		out["input_tokens_per_query"] = optionalAverage(arm.InputTokens, arm.Agg.N)
		out["output_tokens_per_query"] = optionalAverage(arm.OutputTokens, arm.Agg.N)
		out[hit1], out[hit10], out[fmt.Sprintf("nDCG@%d", cutoffs.NDCG)] = s.Hit1, s.Hit10, s.NDCG
		out["latency"] = arm.Lat.Summary()
		out["cost_per_query_usd"] = formatCostPointer(arm.CostUSD, arm.Agg.N)
		return out
	}
	results := make(map[string]any, len(r.Results))
	for mode, shapes := range r.Results {
		byShape := make(map[string]any, len(r.Shapes))
		for _, shape := range r.Shapes {
			if arm := shapes[shape]; arm != nil {
				byShape[shape] = metrics(arm)
			} else {
				byShape[shape] = map[string]any{"status": "unrun"}
			}
		}
		results[mode] = byShape
	}
	return map[string]any{
		"shapes": r.Shapes, "top": r.Top, "max_requests": r.MaxRequests,
		"model": r.Model, "endpoint": r.Endpoint, "preprocess": r.Preprocess,
		"cost_stop_usd": r.CostStopUSD, "input_usd_per_million": r.InputUSDPerM,
		"output_usd_per_million": r.OutputUSDPerM, "complete": r.Complete,
		"failure": nullableString(r.Failure), "results": results,
	}
}

func formatCostPointer(value *float64, queries int) any {
	if value == nil || queries <= 0 {
		return nil
	}
	return *value / float64(queries)
}

func optionalAverage(value *int64, queries int) any {
	if value == nil || queries <= 0 {
		return nil
	}
	return float64(*value) / float64(queries)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func readEvalRerankOptions(cmd *cobra.Command) (evalRerankOptions, error) {
	opts := evalRerankOptions{Top: evalRerankTop, MaxRequests: evalRerankMaxRequests}
	if strings.TrimSpace(evalRerankJev) == "" {
		return opts, nil
	}
	if evalDocKey != "message" {
		return opts, errors.New("--rerank-jev requires --doc-key=message")
	}
	if opts.Top < 1 || opts.Top > typesafeMaxCandidates {
		return opts, fmt.Errorf("--rerank-top must be between 1 and %d", typesafeMaxCandidates)
	}
	if opts.Top > evalLimit {
		return opts, fmt.Errorf("--rerank-top (%d) cannot exceed --limit (%d)", opts.Top, evalLimit)
	}
	if opts.MaxRequests <= 0 {
		return opts, errors.New("--rerank-max-requests must be positive")
	}
	if math.IsNaN(evalRerankCostStopUSD) || math.IsInf(evalRerankCostStopUSD, 0) || evalRerankCostStopUSD <= 0 {
		return opts, errors.New("--rerank-cost-stop-usd must be a positive finite number")
	}
	if cmd != nil && !cmd.Flags().Changed("rerank-input-usd-per-million") {
		return opts, errors.New("--rerank-input-usd-per-million is required when --rerank-jev is enabled")
	}
	if cmd != nil && !cmd.Flags().Changed("rerank-output-usd-per-million") {
		return opts, errors.New("--rerank-output-usd-per-million is required when --rerank-jev is enabled")
	}
	if err := validatePrice("--rerank-input-usd-per-million", evalRerankInputUSDPerM); err != nil {
		return opts, err
	}
	if err := validatePrice("--rerank-output-usd-per-million", evalRerankOutputUSDPerM); err != nil {
		return opts, err
	}
	opts.CostStopUSD = evalRerankCostStopUSD
	opts.InputUSDPerM = evalRerankInputUSDPerM
	opts.OutputUSDPerM = evalRerankOutputUSDPerM
	seen := make(map[string]struct{}, 2)
	for raw := range strings.SplitSeq(evalRerankJev, ",") {
		shape := strings.TrimSpace(raw)
		if shape == "" {
			continue
		}
		if shape != "per-candidate" && shape != "batched" {
			return opts, fmt.Errorf("invalid --rerank-jev value %q (want per-candidate,batched)", shape)
		}
		if _, ok := seen[shape]; ok {
			continue
		}
		seen[shape] = struct{}{}
		opts.Shapes = append(opts.Shapes, shape)
	}
	if len(opts.Shapes) == 0 {
		return opts, errors.New("--rerank-jev must name per-candidate or batched")
	}
	key := os.Getenv("TYPESAFE_API_KEY")
	if strings.TrimSpace(key) == "" {
		return opts, errors.New("TYPESAFE_API_KEY is required when --rerank-jev is enabled")
	}
	opts.APIKey = key
	if cfg != nil {
		vectorConfig := cfg.Vector
		vectorConfig.ApplyDefaults()
		opts.Preprocess = embeddingPreprocessConfig(vectorConfig)
	}
	return opts, nil
}

func validatePrice(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return fmt.Errorf("%s must be a finite nonnegative number", name)
	}
	return nil
}

func validateJevRequestEstimate(topicCount, modeCount int, shapes []string, top, maxRequests int) error {
	if topicCount <= 0 || modeCount <= 0 || len(shapes) == 0 {
		return nil
	}
	if top <= 0 || maxRequests <= 0 {
		return errors.New("invalid rerank request estimate inputs")
	}
	requestsPerTopic := 0
	for _, shape := range shapes {
		requests := 1
		if shape == "per-candidate" {
			requests = top
		} else if shape != "batched" {
			return fmt.Errorf("unknown Jev request shape %q", shape)
		}
		if modeCount > maxRequests/requests || modeCount*requests > maxRequests-requestsPerTopic {
			return fmt.Errorf("--rerank-max-requests (%d) is below the conservative request estimate for %d judged topics across %d modes",
				maxRequests, topicCount, modeCount)
		}
		requestsPerTopic += modeCount * requests
	}
	if requestsPerTopic > 0 && topicCount > maxRequests/requestsPerTopic {
		return fmt.Errorf("--rerank-max-requests (%d) is below the conservative request estimate for %d judged topics across %d modes",
			maxRequests, topicCount, modeCount)
	}
	return nil
}

type rerankBudget struct {
	mu            sync.Mutex
	maxRequests   int
	stopUSD       float64
	inputUSDPerM  float64
	outputUSDPerM float64
	attempts      int
	cost          float64
	unknown       bool
	stopped       bool
	failed        bool
}

func (b *rerankBudget) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed {
		return errors.New("rerank provider failed; no further requests will start")
	}
	if b.unknown {
		return errors.New("rerank usage is unknown; no further requests will start")
	}
	if b.stopped || b.cost >= b.stopUSD {
		b.stopped = true
		return errors.New("rerank local cost stop reached; no further requests will start")
	}
	if b.attempts >= b.maxRequests {
		return errors.New("rerank request limit reached before provider call")
	}
	b.attempts++
	return nil
}

func (b *rerankBudget) preflight(requests int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if requests <= 0 || b.attempts+requests > b.maxRequests {
		return errors.New("rerank request limit reached before provider call")
	}
	if b.failed {
		return errors.New("rerank provider failed; no further requests will start")
	}
	if b.unknown {
		return errors.New("rerank usage is unknown; no further requests will start")
	}
	if b.stopped || b.cost >= b.stopUSD {
		return errors.New("rerank local cost stop reached; no further requests will start")
	}
	return nil
}

func (b *rerankBudget) record(usage rerank.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		b.unknown = true
		return
	}
	b.cost += float64(*usage.InputTokens)*b.inputUSDPerM/1e6 +
		float64(*usage.OutputTokens)*b.outputUSDPerM/1e6
	if b.cost >= b.stopUSD {
		b.stopped = true
	}
}

func (b *rerankBudget) fail() {
	b.mu.Lock()
	b.failed = true
	b.mu.Unlock()
}

type jevReranker struct {
	shape  string
	key    string
	client *http.Client
	budget *rerankBudget
}

func newEvalJevReranker(shape, key string, budget *rerankBudget) (rerank.Reranker, error) {
	return newJevReranker(shape, key, budget)
}

func newJevReranker(shape, key string, budget *rerankBudget) (*jevReranker, error) {
	if shape != "per-candidate" && shape != "batched" {
		return nil, fmt.Errorf("unknown Jev request shape %q", shape)
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("TYPESAFE_API_KEY is required")
	}
	if budget == nil {
		return nil, errors.New("reranker budget is required")
	}
	return &jevReranker{
		shape: shape,
		key:   key,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
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
		return nil, errors.New("query exceeds the 4096-byte Jev limit or is empty")
	}
	if len(candidates) == 0 || len(candidates) > typesafeMaxCandidates {
		return nil, fmt.Errorf("candidate count must be between 1 and %d", typesafeMaxCandidates)
	}
	for i, candidate := range candidates {
		if !utf8.ValidString(candidate) || len([]byte(candidate)) > typesafeMaxCandidate {
			return nil, fmt.Errorf("candidate %d exceeds the 2048-byte Jev limit", i)
		}
	}
	var requests []jevRequest
	switch shape {
	case "per-candidate":
		requests = make([]jevRequest, len(candidates))
		for i, candidate := range candidates {
			requests[i] = jevRequest{
				State:     jevPerCandidateState{Query: query, Candidate: candidate},
				Model:     typesafeModel,
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
			Model: typesafeModel, Questions: questions,
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
			return nil, errors.New("encoded Jev request exceeds 131072 bytes")
		}
		out[i] = body
	}
	return out, nil
}

func decodeJevResponse(data []byte, questionIDs []string) (rerank.Result, error) {
	var response jevResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return rerank.Result{}, errors.New("invalid Jev response")
	}
	if response.Model != typesafeModel || len(response.Answers) != len(questionIDs) {
		return rerank.Result{}, errors.New("reranker response model or answer count did not match request")
	}
	wanted := make(map[string]struct{}, len(questionIDs))
	scores := make([]float64, len(questionIDs))
	for i, id := range questionIDs {
		wanted[id] = struct{}{}
		answer, ok := response.Answers[id]
		if !ok || answer.Type != "noul" || answer.Noul == nil || math.IsNaN(*answer.Noul) ||
			math.IsInf(*answer.Noul, 0) || *answer.Noul < 0 || *answer.Noul > 1 {
			return rerank.Result{}, errors.New("reranker response contained an invalid answer")
		}
		scores[i] = *answer.Noul
	}
	for id := range response.Answers {
		if _, ok := wanted[id]; !ok {
			return rerank.Result{}, errors.New("reranker response contained an unexpected answer")
		}
	}
	var usage rerank.Usage
	if response.Usage != nil {
		if response.Usage.InputTokens != nil && *response.Usage.InputTokens < 0 ||
			response.Usage.OutputTokens != nil && *response.Usage.OutputTokens < 0 {
			return rerank.Result{}, errors.New("reranker response contained invalid token usage")
		}
		usage.InputTokens = validTokenPointer(response.Usage.InputTokens)
		usage.OutputTokens = validTokenPointer(response.Usage.OutputTokens)
	}
	usage.Complete = usage.InputTokens != nil && usage.OutputTokens != nil
	return rerank.Result{Scores: scores, Usage: usage}, nil
}

func validTokenPointer(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	usage := *value
	return &usage
}

func (j *jevReranker) Rerank(ctx context.Context, request rerank.Request) (rerank.Result, error) {
	if ctx == nil {
		return emptyJevResult(), errors.New("reranker context is required")
	}
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
	result := rerank.Result{Scores: scores, Usage: rerank.Usage{
		Requests: attempted, InputTokens: &totalInput, OutputTokens: &totalOutput, Complete: usageComplete,
	}}
	if groupErr != nil {
		return result, fmt.Errorf("rerank requests failed: %w", groupErr)
	}
	return result, nil
}

func emptyJevResult() rerank.Result {
	input, output := int64(0), int64(0)
	return rerank.Result{Usage: rerank.Usage{InputTokens: &input, OutputTokens: &output, Complete: true}}
}

func safeRerankFailure(err error) string {
	if err == nil {
		return "provider request failed"
	}
	message := err.Error()
	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
		message = cause.Error()
	}
	switch {
	case strings.Contains(message, "request limit reached"):
		return "request limit reached"
	case strings.Contains(message, "local cost stop reached"):
		return "local cost stop reached"
	case strings.Contains(message, "usage is unknown"):
		return "provider usage unavailable"
	case strings.Contains(message, "timed out or was canceled"):
		return "provider timeout or cancellation"
	case strings.HasPrefix(message, "provider returned HTTP "):
		status, err := strconv.Atoi(strings.TrimPrefix(message, "provider returned HTTP "))
		if err == nil && status >= 100 && status <= 599 {
			return fmt.Sprintf("provider returned HTTP %d", status)
		}
		return "provider request failed"
	case strings.Contains(message, "query exceeds") || strings.Contains(message, "candidate count") ||
		strings.Contains(message, "candidate ") && strings.Contains(message, "exceeds") ||
		strings.Contains(message, "encoded Jev request exceeds"):
		return "request bounds exceeded"
	case strings.Contains(message, "response") || strings.Contains(message, "Jev response"):
		return "invalid provider response"
	case strings.Contains(message, "candidate preparation"):
		return "candidate preparation failed"
	default:
		return "provider request failed"
	}
}

func (j *jevReranker) send(ctx context.Context, body []byte, ids []string) (rerank.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, typesafeEndpoint, bytes.NewReader(body))
	if err != nil {
		return rerank.Result{}, errors.New("construct Jev request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.key)
	response, err := j.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return rerank.Result{}, errors.New("provider request timed out or was canceled")
		}
		return rerank.Result{}, errors.New("provider request failed")
	}
	if response == nil || response.Body == nil {
		return rerank.Result{}, errors.New("provider returned no response body")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return rerank.Result{}, fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}
	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return rerank.Result{}, errors.New("provider returned a non-JSON response")
	}
	limited := io.LimitReader(response.Body, typesafeMaxResponse+1)
	body, err = io.ReadAll(limited)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return rerank.Result{}, errors.New("provider request timed out or was canceled")
		}
		return rerank.Result{}, errors.New("read Jev response")
	}
	if len(body) > typesafeMaxResponse {
		return rerank.Result{}, errors.New("provider response exceeds 65536 bytes")
	}
	return decodeJevResponse(body, ids)
}

func truncateUTF8Bytes(value string, limit int) string {
	if len([]byte(value)) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func prepareEvalCandidates(ctx context.Context, s *store.Store, keys []string, hits map[string]evalHit, preprocess embed.PreprocessConfig, top int) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	top = min(top, len(keys))
	texts := make([]string, top)
	for i, key := range keys[:top] {
		hit, ok := hits[key]
		if !ok || hit.MessageID == 0 {
			return nil, fmt.Errorf("rerank candidate %d has no message identity", i)
		}
		message, err := s.GetMessageContext(ctx, hit.MessageID)
		if err != nil {
			return nil, fmt.Errorf("prepare rerank candidate %d: %w", i, err)
		}
		body := embed.BodyTextForEmbedding(message.BodyText, message.BodyHTML)
		text, _ := embed.Preprocess(message.Subject, body, 0, preprocess)
		texts[i] = truncateUTF8Bytes(text, typesafeMaxCandidate)
	}
	return texts, nil
}

func rerankEvalKeys(ctx context.Context, scorer rerank.Reranker, query string, keys, texts []string) ([]string, rerank.Result, error) {
	if len(keys) < 2 || len(texts) < 2 {
		return slices.Clone(keys), rerank.Result{}, nil
	}
	texts = texts[:min(len(texts), len(keys))]
	result, err := scorer.Rerank(ctx, rerank.Request{Query: query, Candidates: texts})
	if err != nil {
		return nil, result, err
	}
	if len(result.Scores) != len(texts) {
		return nil, result, fmt.Errorf("reranker returned %d scores for %d candidates", len(result.Scores), len(texts))
	}
	order, err := rerank.Order(result.Scores)
	if err != nil {
		return nil, result, err
	}
	out := slices.Clone(keys)
	prefix := slices.Clone(keys[:len(texts)])
	for i, index := range order {
		out[i] = prefix[index]
	}
	return out, result, nil
}
