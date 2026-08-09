//go:build sqlite_vec

package main

import (
	"container/heap"
	"math"
	"math/rand"
	"sort"
	"time"
)

type topKItem struct {
	id    string
	score float64
}

type worstFirst []topKItem

func (h *worstFirst) Len() int { return len(*h) }
func (h *worstFirst) Less(i, j int) bool {
	if (*h)[i].score == (*h)[j].score {
		return (*h)[i].id > (*h)[j].id
	}
	return (*h)[i].score < (*h)[j].score
}
func (h *worstFirst) Swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *worstFirst) Push(value any) {
	item, ok := value.(topKItem)
	if !ok {
		panic("worstFirst received a non-topKItem value")
	}
	*h = append(*h, item)
}
func (h *worstFirst) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

func ndcgAt10(retrievedGrades, allJudgedGrades []int) float64 {
	const k = 10
	if k <= 0 || len(retrievedGrades) == 0 || len(allJudgedGrades) == 0 {
		return 0
	}
	dcg := discountedGain(retrievedGrades[:min(k, len(retrievedGrades))])
	ideal := append([]int(nil), allJudgedGrades...)
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := discountedGain(ideal[:min(k, len(ideal))])
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func gradeValues(grades map[string]int) []int {
	values := make([]int, 0, len(grades))
	for _, grade := range grades {
		values = append(values, grade)
	}
	return values
}

func discountedGain(grades []int) float64 {
	var total float64
	for i, grade := range grades {
		if grade <= 0 {
			continue
		}
		total += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(i+2))
	}
	return total
}

func recallAt(grades []int, k, relevantTotal int) float64 {
	if relevantTotal <= 0 || k <= 0 {
		return 0
	}
	retrieved := 0
	for _, grade := range grades[:min(k, len(grades))] {
		if grade > 0 {
			retrieved++
		}
	}
	return float64(retrieved) / float64(relevantTotal)
}

func reciprocalRankAt(grades []int, k int) float64 {
	for i, grade := range grades[:min(max(k, 0), len(grades))] {
		if grade > 0 {
			return 1 / float64(i+1)
		}
	}
	return 0
}

func hitAt(grades []int, k int) float64 {
	for _, grade := range grades[:min(max(k, 0), len(grades))] {
		if grade > 0 {
			return 1
		}
	}
	return 0
}

func gradesForRanking(ranking []string, judgment Judgment) []int {
	grades := make([]int, len(ranking))
	for i, id := range ranking {
		grades[i] = judgment.Grades[id]
	}
	return grades
}

func evidenceHitAt(ranking, evidence []string, k int) float64 {
	wanted := make(map[string]struct{}, len(evidence))
	for _, id := range evidence {
		wanted[id] = struct{}{}
	}
	for _, id := range ranking[:min(max(k, 0), len(ranking))] {
		if _, ok := wanted[id]; ok {
			return 1
		}
	}
	return 0
}

func vectorNorm(vector []float32) float64 {
	var sum float64
	for _, value := range vector {
		sum += float64(value) * float64(value)
	}
	return math.Sqrt(sum)
}

func similarityCosine(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return math.Inf(-1)
	}
	var dot, leftNorm, rightNorm float64
	for i := range left {
		x, y := float64(left[i]), float64(right[i])
		dot += x * y
		leftNorm += x * x
		rightNorm += y * y
	}
	if leftNorm == 0 || rightNorm == 0 {
		return math.Inf(-1)
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func similarityL2(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return math.Inf(-1)
	}
	var squared float64
	for i := range left {
		delta := float64(left[i] - right[i])
		squared += delta * delta
	}
	return -math.Sqrt(squared)
}

// exactTopKChunks scans every stored vector but retains only K source scores.
// This is the exact oracle used at the 100k scale.
func exactTopKChunks(vectors map[string][][]float32, query []float32, similarity func([]float32, []float32) float64, k int) []string {
	if k <= 0 {
		return nil
	}
	h := &worstFirst{}
	heap.Init(h)
	for id, chunks := range vectors {
		best := math.Inf(-1)
		for _, chunk := range chunks {
			best = max(best, similarity(query, chunk))
		}
		item := topKItem{id: id, score: best}
		if h.Len() < k {
			heap.Push(h, item)
			continue
		}
		worst := (*h)[0]
		if item.score > worst.score || (item.score == worst.score && item.id < worst.id) {
			heap.Pop(h)
			heap.Push(h, item)
		}
	}
	items := make([]topKItem, h.Len())
	copy(items, *h)
	sort.Slice(items, func(i, j int) bool {
		if items[i].score == items[j].score {
			return items[i].id < items[j].id
		}
		return items[i].score > items[j].score
	})
	result := make([]string, len(items))
	for i, item := range items {
		result[i] = item.id
	}
	return result
}

func topKOverlap(left, right []string, k int) float64 {
	if k <= 0 {
		return 0
	}
	leftLimit, rightLimit := min(k, len(left)), min(k, len(right))
	if leftLimit == 0 || rightLimit == 0 {
		return 0
	}
	set := make(map[string]struct{}, leftLimit)
	for _, id := range left[:leftLimit] {
		set[id] = struct{}{}
	}
	overlap := 0
	for _, id := range right[:rightLimit] {
		if _, ok := set[id]; ok {
			overlap++
		}
	}
	return float64(overlap) / float64(min(leftLimit, rightLimit))
}

type ScenarioPair struct {
	ScenarioID string
	Family     string
	Baseline   float64
	Candidate  float64
}

type Interval struct {
	Point float64 `json:"point"`
	Lower float64 `json:"lower_95"`
	Upper float64 `json:"upper_95"`
}

func pairedBootstrap(pairs []ScenarioPair, samples int, seed int64) Interval {
	if len(pairs) == 0 {
		return Interval{}
	}
	var point float64
	for _, pair := range pairs {
		point += pair.Candidate - pair.Baseline
	}
	point /= float64(len(pairs))
	if samples <= 0 {
		return Interval{Point: point, Lower: point, Upper: point}
	}
	// #nosec G404 -- paired bootstrap must be deterministic for reproducible evaluation reports.
	rng := rand.New(rand.NewSource(seed))
	deltas := make([]float64, samples)
	for sample := range samples {
		var delta float64
		for range pairs {
			pair := pairs[rng.Intn(len(pairs))]
			delta += pair.Candidate - pair.Baseline
		}
		deltas[sample] = delta / float64(len(pairs))
	}
	sort.Float64s(deltas)
	return Interval{Point: point, Lower: percentile(deltas, 0.025), Upper: percentile(deltas, 0.975)}
}

func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if quantile <= 0 {
		return sorted[0]
	}
	if quantile >= 1 {
		return sorted[len(sorted)-1]
	}
	position := quantile * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

type RankingMetrics struct {
	NDCGAt10           float64 `json:"ndcg_at_10"`
	RecallAt10         float64 `json:"recall_at_10"`
	RecallAt20         float64 `json:"recall_at_20"`
	MRRAt10            float64 `json:"mrr_at_10"`
	HitAt5             float64 `json:"hit_at_5"`
	EvidenceHitAt10    float64 `json:"transcript_evidence_hit_at_10"`
	ExactANNRecallAt10 float64 `json:"exact_ann_recall_at_10"`
	L2CosineOverlap10  float64 `json:"l2_cosine_overlap_at_10"`
}

type LatencySummary struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

func summarizeLatencies(values []time.Duration) LatencySummary {
	millis := make([]float64, len(values))
	for i, value := range values {
		millis[i] = float64(value) / float64(time.Millisecond)
	}
	sort.Float64s(millis)
	return LatencySummary{P50: percentile(millis, 0.5), P95: percentile(millis, 0.95), P99: percentile(millis, 0.99)}
}

type ResourceSummary struct {
	WallMillis        float64 `json:"wall_ms"`
	PeakRSSBytes      *int64  `json:"peak_rss_bytes"`
	MemoryMeasurement string  `json:"memory_measurement"`
	IndexBytes        int64   `json:"index_bytes"`
}

type ArmReport struct {
	Model                 string                    `json:"model"`
	Requests              int64                     `json:"requests"`
	DocumentRequests      int64                     `json:"document_requests"`
	QueryRequests         int64                     `json:"query_requests"`
	QueryCacheGroup       string                    `json:"query_cache_group,omitempty"`
	RequestAccounting     string                    `json:"request_accounting"`
	InputTokens           int64                     `json:"input_tokens"`
	QueryInputTokens      int64                     `json:"query_input_tokens"`
	TokenAccounting       string                    `json:"token_accounting"`
	SuccessfulResponses   int64                     `json:"successful_responses"`
	UsageResponses        int64                     `json:"usage_responses"`
	UsageAvailable        bool                      `json:"usage_available"`
	Errors                int64                     `json:"errors"`
	HTTPStatuses          map[int]int64             `json:"http_statuses,omitempty"`
	ErrorMessages         []string                  `json:"error_messages,omitempty"`
	LatencyMillis         LatencySummary            `json:"latency_ms"`
	QueryLatencyMillis    LatencySummary            `json:"query_latency_ms"`
	DocumentLatencyMillis LatencySummary            `json:"document_latency_ms"`
	Build                 ResourceSummary           `json:"build"`
	Macro                 RankingMetrics            `json:"macro"`
	ByFamily              map[string]RankingMetrics `json:"by_family,omitempty"`
	VectorNorms           LatencySummary            `json:"vector_norms"`
	Config                ArmConfigReport           `json:"config"`
	latencySamples        []time.Duration
}

type ArmConfigReport struct {
	GenerationFingerprint string `json:"generation_fingerprint"`
	MaxInputChars         int    `json:"max_input_chars"`
	BatchSize             int    `json:"batch_size"`
	APIFormat             string `json:"api_format"`
	BuilderBoundary       string `json:"builder_boundary"`
}

type QualitySummary struct {
	ContextualMacro    Interval            `json:"contextual_macro_ndcg_delta"`
	ContextualByFamily map[string]Interval `json:"contextual_family_ndcg_delta"`
	EndToEnd           Interval            `json:"end_to_end_ndcg_delta"`
	EmailOrIndependent Interval            `json:"email_or_context_independent_ndcg_delta"`
	HybridContextual   Interval            `json:"hybrid_contextual_ndcg_delta"`
	HybridOverall      Interval            `json:"hybrid_overall_ndcg_delta"`
}

type OperationalSummary struct {
	ExactEvaluated           bool     `json:"exact_evaluated"`
	ANNRecallAt10            float64  `json:"ann_recall_at_10"`
	CachedANNP95Ratio        float64  `json:"cached_ann_p95_ratio"`
	EndToEndP95Ratio         float64  `json:"end_to_end_p95_ratio"`
	EndToEndP95Millis        float64  `json:"end_to_end_p95_ms"`
	RebuildTimeRatio         float64  `json:"rebuild_time_ratio"`
	PeakRSSRatio             *float64 `json:"peak_rss_ratio"`
	IndexSizeRatio           float64  `json:"index_size_ratio"`
	ProviderTokenRatio       float64  `json:"provider_token_ratio"`
	ProviderTokensAvailable  bool     `json:"provider_tokens_available"`
	RequestErrorRate         float64  `json:"request_error_rate"`
	AppendTokensAvailable    bool     `json:"append_tokens_available"`
	AppendTokenAmplification float64  `json:"append_token_amplification"`
}

type GateResult struct {
	Name      string  `json:"name"`
	Passed    bool    `json:"passed"`
	Evaluated bool    `json:"evaluated"`
	Value     float64 `json:"value"`
	Limit     string  `json:"limit"`
}

type GateSummary struct {
	Passed  bool         `json:"passed"`
	Results []GateResult `json:"results"`
}

type EvaluationReport struct {
	SchemaVersion     int                  `json:"schema_version"`
	Complete          bool                 `json:"complete"`
	RunErrors         []string             `json:"run_errors,omitempty"`
	RunID             string               `json:"run_id"`
	Commit            string               `json:"commit"`
	CorpusHash        string               `json:"corpus_hash"`
	QueryHash         string               `json:"query_hash"`
	PolicyFingerprint string               `json:"policy_fingerprint"`
	SourceRows        int                  `json:"source_rows"`
	FTSRows           int                  `json:"fts_rows"`
	GeneratedAt       string               `json:"generated_at"`
	Arms              map[string]ArmReport `json:"arms"`
	Quality           QualitySummary       `json:"quality"`
	Operational       OperationalSummary   `json:"operational"`
	Gates             GateSummary          `json:"gates"`
	Exact             ExactDiagnostics     `json:"exact_diagnostics"`
}

type ExactDiagnostics struct {
	Enabled            bool   `json:"enabled"`
	Scale              int    `json:"scale"`
	ANNOracle          string `json:"ann_oracle"`
	CosineDiagnostic   string `json:"cosine_diagnostic"`
	TopK               int    `json:"top_k"`
	ScaleGateEvaluated bool   `json:"scale_gate_evaluated"`
}

func exactDiagnosticPolicy(enabled bool, scale int) ExactDiagnostics {
	return ExactDiagnostics{Enabled: enabled, Scale: scale, ANNOracle: "exhaustive_l2",
		CosineDiagnostic: "l2_cosine_top_k_overlap", TopK: 10, ScaleGateEvaluated: true}
}

func newReport(runID, commit, corpusHash, queryHash string) EvaluationReport {
	return EvaluationReport{
		SchemaVersion: 1, Complete: true, RunID: runID, Commit: commit, CorpusHash: corpusHash, QueryHash: queryHash,
		PolicyFingerprint: evaluationPolicyFingerprint(),
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339), Arms: make(map[string]ArmReport),
		Quality: QualitySummary{ContextualByFamily: make(map[string]Interval)},
	}
}

func (r *EvaluationReport) evaluateGates() {
	peakRSSValue := 0.0
	peakRSSPassed := false
	peakRSSEvaluated := r.Operational.PeakRSSRatio != nil
	if peakRSSEvaluated {
		peakRSSValue = *r.Operational.PeakRSSRatio
		peakRSSPassed = peakRSSValue <= 1.25
	}
	results := []GateResult{
		{Name: "contextual_macro_point", Evaluated: true, Passed: r.Quality.ContextualMacro.Point >= 0.03, Value: r.Quality.ContextualMacro.Point, Limit: ">=0.03"},
		{Name: "contextual_macro_lower", Passed: r.Quality.ContextualMacro.Lower > 0, Value: r.Quality.ContextualMacro.Lower, Limit: ">0"},
		{Name: "chat_lower", Passed: r.Quality.ContextualByFamily[familyChat].Lower > 0, Value: r.Quality.ContextualByFamily[familyChat].Lower, Limit: ">0"},
		{Name: "transcript_lower", Passed: r.Quality.ContextualByFamily[familyTranscript].Lower > 0, Value: r.Quality.ContextualByFamily[familyTranscript].Lower, Limit: ">0"},
		{Name: "end_to_end_point", Passed: r.Quality.EndToEnd.Point >= 0.05, Value: r.Quality.EndToEnd.Point, Limit: ">=0.05"},
		{Name: "email_independent_lower", Passed: r.Quality.EmailOrIndependent.Lower > -0.02, Value: r.Quality.EmailOrIndependent.Lower, Limit: ">-0.02"},
		{Name: "hybrid_contextual_point", Passed: r.Quality.HybridContextual.Point >= 0.02, Value: r.Quality.HybridContextual.Point, Limit: ">=0.02"},
		{Name: "hybrid_overall_lower", Passed: r.Quality.HybridOverall.Lower > -0.01, Value: r.Quality.HybridOverall.Lower, Limit: ">-0.01"},
		{Name: "ann_recall", Evaluated: r.Operational.ExactEvaluated, Passed: r.Operational.ExactEvaluated && r.Operational.ANNRecallAt10 >= 0.99, Value: r.Operational.ANNRecallAt10, Limit: ">=0.99"},
		{Name: "cached_ann_p95", Passed: r.Operational.CachedANNP95Ratio <= 1.10, Value: r.Operational.CachedANNP95Ratio, Limit: "<=1.10"},
		{Name: "end_to_end_p95_ratio", Passed: r.Operational.EndToEndP95Ratio <= 1.25, Value: r.Operational.EndToEndP95Ratio, Limit: "<=1.25"},
		{Name: "end_to_end_p95_ms", Passed: r.Operational.EndToEndP95Millis <= 1000, Value: r.Operational.EndToEndP95Millis, Limit: "<=1000"},
		{Name: "rebuild_time", Passed: r.Operational.RebuildTimeRatio <= 1.5, Value: r.Operational.RebuildTimeRatio, Limit: "<=1.5"},
		{Name: "peak_rss_ratio", Evaluated: peakRSSEvaluated, Passed: peakRSSPassed, Value: peakRSSValue, Limit: "<=1.25"},
		{Name: "index_size", Passed: r.Operational.IndexSizeRatio <= 1.25, Value: r.Operational.IndexSizeRatio, Limit: "<=1.25"},
		{Name: "provider_tokens", Evaluated: r.Operational.ProviderTokensAvailable,
			Passed: r.Operational.ProviderTokensAvailable && r.Operational.ProviderTokenRatio <= 1.25,
			Value:  r.Operational.ProviderTokenRatio, Limit: "<=1.25"},
		{Name: "request_errors", Passed: r.Operational.RequestErrorRate < 0.001, Value: r.Operational.RequestErrorRate, Limit: "<0.001"},
		{Name: "append_tokens", Evaluated: r.Operational.AppendTokensAvailable,
			Passed: r.Operational.AppendTokensAvailable && r.Operational.AppendTokenAmplification <= 5,
			Value:  r.Operational.AppendTokenAmplification, Limit: "<=5"},
	}
	passed := true
	for i, result := range results {
		if result.Name != "peak_rss_ratio" && result.Name != "ann_recall" && result.Name != "provider_tokens" && result.Name != "append_tokens" {
			results[i].Evaluated = true
		}
		passed = passed && results[i].Evaluated && results[i].Passed
	}
	r.Gates = GateSummary{Passed: passed, Results: results}
}
