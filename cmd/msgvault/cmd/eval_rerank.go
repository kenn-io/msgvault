//go:build sqlite_vec

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/rerank"
)

const typesafeRunTimeout = 30 * time.Minute

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

type evalReranker interface {
	Rerank(ctx context.Context, request rerank.Request) (rerank.Result, error)
}

type evalRerankerFactory func(string, string, *rerank.Budget) (evalReranker, error)

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
	scorers       map[string]evalReranker
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
		Model: rerank.JevModel, Endpoint: rerank.JevEndpoint, Preprocess: options.Preprocess,
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
	if opts.Top < 2 || opts.Top > rerank.MaxCandidates {
		return opts, fmt.Errorf("--rerank-top must be between 2 and %d", rerank.MaxCandidates)
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
		texts[i] = truncateUTF8Bytes(text, rerank.MaxCandidateBytes)
	}
	return texts, nil
}

func rerankEvalKeys(ctx context.Context, scorer evalReranker, query string, keys, texts []string) ([]string, rerank.Result, error) {
	if len(keys) < 2 || len(texts) < 2 {
		return slices.Clone(keys), rerank.Result{Usage: rerank.Usage{
			InputTokens: new(int64(0)), OutputTokens: new(int64(0)), Complete: true,
		}}, nil
	}
	texts = texts[:min(len(texts), len(keys))]
	result, err := scorer.Rerank(ctx, rerank.Request{Query: query, Candidates: texts})
	if err != nil {
		return nil, result, err
	}
	if len(result.Scores) != len(texts) {
		return nil, result, fmt.Errorf("%w: returned %d scores for %d candidates", rerank.ErrInvalidResponse, len(result.Scores), len(texts))
	}
	order, err := rerank.Order(result.Scores)
	if err != nil {
		return nil, result, fmt.Errorf("%w: %w", rerank.ErrInvalidResponse, err)
	}
	out := slices.Clone(keys)
	prefix := slices.Clone(keys[:len(texts)])
	for i, index := range order {
		out[i] = prefix[index]
	}
	return out, result, nil
}

func (r *evalRerankReport) fail(mode, shape, reason string) {
	arm := r.arm(mode, shape)
	arm.Status, arm.Complete, arm.Error = "failed", false, reason
	r.Complete, r.Failure = false, reason
}

func (r *evalRerankReport) scoreRanking(ctx context.Context, s *store.Store, hits map[string]evalHit,
	mode string, topic eval.Topic, ranked []string, rel map[string]struct{}, cutoffs eval.Cutoffs, elapsed time.Duration,
) error {
	prepStart := time.Now()
	var texts []string
	if len(ranked) >= 2 {
		var err error
		texts, err = prepareEvalCandidates(ctx, s, ranked, hits, r.Preprocess, r.Top)
		if err != nil {
			for _, shape := range r.Shapes {
				r.fail(mode, shape, "candidate preparation failed")
			}
			return fmt.Errorf("topic %s, mode %s: candidate preparation failed", topic.ID, mode)
		}
	}
	prepElapsed := time.Since(prepStart)
	for _, shape := range r.Shapes {
		arm := r.arm(mode, shape)
		providerStart := time.Now()
		reranked, result, err := rerankEvalKeys(ctx, r.scorers[shape], topic.Query, ranked, texts)
		arm.addUsage(result.Usage, r.InputUSDPerM, r.OutputUSDPerM)
		if err != nil {
			failure := rerank.SafeFailure(err)
			r.fail(mode, shape, failure)
			return fmt.Errorf("topic %s, mode %s, shape %s: %s", topic.ID, mode, shape, failure)
		}
		latency := elapsed
		if len(texts) >= 2 {
			latency += prepElapsed + time.Since(providerStart)
		}
		arm.addQuality(reranked, rel, cutoffs, latency)
	}
	return nil
}

func (r *evalRerankReport) reconcile(modes []string, baseline map[string]*eval.Aggregate) {
	for _, mode := range modes {
		for _, shape := range r.Shapes {
			if r.Results[mode][shape] == nil {
				arm := r.arm(mode, shape)
				arm.Status, arm.Complete = "unrun", false
				r.Complete = false
				continue
			}
			arm := r.Results[mode][shape]
			if arm.Status != "failed" && (arm.Agg == nil || arm.Agg.N != baseline[mode].N) {
				arm.Status, arm.Complete = "incomplete", false
				r.Complete = false
			}
		}
	}
}
