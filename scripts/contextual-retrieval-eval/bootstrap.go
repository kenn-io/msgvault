//go:build sqlite_vec

package main

import (
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector/embed"
)

type runConfig struct {
	ScenarioPath string
	JudgmentPath string
	Distractors  int
	Bootstrap    int
	APIKey       string
	Endpoint     string
	WorkDir      string
	RunID        string
	Commit       string
	ExactOracle  bool
}

type scenarioResult struct {
	Scenario Scenario
	ByArm    map[string]rankingResult
}

type rankingResult struct {
	ANN        []string       `json:"ann"`
	Exact      []string       `json:"exact"`
	L2         []string       `json:"l2"`
	Hybrid     []string       `json:"hybrid"`
	Metrics    RankingMetrics `json:"metrics"`
	HybridNDCG float64        `json:"hybrid_ndcg"`
	ANNTime    time.Duration  `json:"ann_time"`
	FullTime   time.Duration  `json:"full_time"`
}

type evaluationRun struct {
	Report            EvaluationReport                                 `json:"report"`
	Rankings          map[string]map[string][]string                   `json:"rankings"`
	HybridRankings    map[string]map[string][]string                   `json:"hybrid_rankings"`
	Queries           map[string]string                                `json:"queries"`
	Sources           map[string]poolSource                            `json:"sources"`
	WinningProvenance map[string]map[string]map[string][]chunkEvidence `json:"winning_provenance"`
}

type poolSource struct {
	Excerpt    string          `json:"excerpt"`
	Provenance chunkProvenance `json:"provenance"`
	MessageID  int64           `json:"message_id"`
	ChunkID    string          `json:"chunk_id"`
	DocumentID string          `json:"document_id"`
}

func executeEvaluation(ctx context.Context, cfg runConfig) (evaluationRun, error) {
	corpus, err := loadCorpus(cfg.ScenarioPath, cfg.JudgmentPath, cfg.Distractors)
	if err != nil {
		return evaluationRun{}, err
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.voyageai.com/v1"
	}
	proxyHandler, observer, err := newCountingEmbeddingProxy(cfg.Endpoint)
	if err != nil {
		return evaluationRun{}, fmt.Errorf("create embedding attempt proxy: %w", err)
	}
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()
	physicalEndpoint := proxyServer.URL
	if cfg.WorkDir == "" {
		cfg.WorkDir, err = os.MkdirTemp("", "msgvault-contextual-eval-")
		if err != nil {
			return evaluationRun{}, err
		}
		defer func() { _ = os.RemoveAll(cfg.WorkDir) }()
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return evaluationRun{}, err
	}
	sourceDir := filepath.Join(cfg.WorkDir, "source")
	sources, mainDB, mainPath, cleanupSource, err := assembleStructuredCorpus(ctx, sourceDir, corpus)
	if err != nil {
		return evaluationRun{}, err
	}
	defer cleanupSource()
	corpusHash := materializedCorpusHash(corpus, sources, evaluationPolicyFingerprint())
	var sourceRows, ftsRows int
	if err := mainDB.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&sourceRows); err != nil {
		return evaluationRun{}, fmt.Errorf("count evaluator source rows: %w", err)
	}
	if err := mainDB.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts`).Scan(&ftsRows); err != nil {
		return evaluationRun{}, fmt.Errorf("count evaluator FTS rows: %w", err)
	}

	oldClient := embed.NewClient(embed.Config{
		Endpoint: physicalEndpoint, APIKey: cfg.APIKey, Model: oldProductionModel,
		Dimension: evaluationDimension, Timeout: 60 * time.Second, MaxRetries: 3,
	})
	contextClient := embed.NewVoyageClient(embed.VoyageConfig{
		Endpoint: physicalEndpoint, APIKey: cfg.APIKey, Model: context4Model,
		Dimension: evaluationDimension, Timeout: 60 * time.Second, MaxRetries: 3,
	})
	queryVectors := newQueryCache(contextClient)

	arms := []string{ArmOldProduction, ArmOldContext4Singleton, ArmStructuredSingleton, ArmNestedContext4}
	armReports := make(map[string]ArmReport, len(arms))
	oldQueries := make(map[string][]float32, len(corpus.Scenarios))
	contextQueries := make(map[string][]float32, len(corpus.Scenarios))
	oldQueryTimes := make(map[string]time.Duration, len(corpus.Scenarios))
	contextQueryTimes := make(map[string]time.Duration, len(corpus.Scenarios))
	oldQueryHTTPBefore := observer.Snapshot()
	for _, scenario := range corpus.Scenarios {
		start := time.Now()
		oldQueries[scenario.ID], err = oldClient.EmbedQuery(ctx, scenario.Query)
		oldQueryTimes[scenario.ID] = time.Since(start)
		if err != nil {
			delta := httpAttemptDelta(oldQueryHTTPBefore, observer.Snapshot())
			report := observedArmReport(ArmOldProduction)
			applyHTTPObservation(&report, delta, "query")
			armReports[ArmOldProduction] = report
			message := fmt.Sprintf("embed O-prod query %s failed", scenario.ID)
			return partialEvaluationRun(cfg, corpusHash, corpus.QueryHash(), armReports, message), fmt.Errorf("%s", message)
		}
	}
	oldQueryHTTP := httpAttemptDelta(oldQueryHTTPBefore, observer.Snapshot())
	contextQueryHTTPBefore := observer.Snapshot()
	for _, scenario := range corpus.Scenarios {
		start := time.Now()
		contextQueries[scenario.ID], err = queryVectors.ForArm(ctx, ArmNestedContext4, scenario.Query)
		contextQueryTimes[scenario.ID] = time.Since(start)
		if err != nil {
			oldReport := observedArmReport(ArmOldProduction)
			applyHTTPObservation(&oldReport, oldQueryHTTP, "query")
			armReports[ArmOldProduction] = oldReport
			delta := httpAttemptDelta(contextQueryHTTPBefore, observer.Snapshot())
			contextReport := observedArmReport(ArmNestedContext4)
			applyHTTPObservation(&contextReport, delta, "query")
			armReports[ArmNestedContext4] = contextReport
			message := fmt.Sprintf("embed Context 4 query %s failed", scenario.ID)
			return partialEvaluationRun(cfg, corpusHash, corpus.QueryHash(), armReports, message), fmt.Errorf("%s", message)
		}
	}
	contextQueryHTTP := httpAttemptDelta(contextQueryHTTPBefore, observer.Snapshot())

	results := make([]scenarioResult, len(corpus.Scenarios))
	resultByID := make(map[string]*scenarioResult, len(corpus.Scenarios))
	for i, scenario := range corpus.Scenarios {
		results[i] = scenarioResult{Scenario: scenario, ByArm: make(map[string]rankingResult, len(arms))}
		resultByID[scenario.ID] = &results[i]
	}
	rankings := make(map[string]map[string][]string, len(arms))
	hybridRankings := make(map[string]map[string][]string, len(arms))
	winningProvenance := make(map[string]map[string]map[string][]chunkEvidence, len(arms))
	for _, arm := range arms {
		input := armEvalInput{Documents: buildArmDocuments(arm, sources), Exact: cfg.ExactOracle}
		for _, scenario := range corpus.Scenarios {
			query, queryTime := contextQueries[scenario.ID], time.Duration(0)
			switch arm {
			case ArmOldProduction:
				query, queryTime = oldQueries[scenario.ID], oldQueryTimes[scenario.ID]
			case ArmNestedContext4:
				queryTime = contextQueryTimes[scenario.ID]
			}
			input.Scenarios = append(input.Scenarios, armScenarioInput{Scenario: scenario,
				Judgment: corpus.Judgments[scenario.ID], Query: query, QueryTime: queryTime})
		}
		beforeHTTP := observer.Snapshot()
		client := documentEmbedder(contextClient)
		switch arm {
		case ArmOldProduction:
			client = oldClient
		}
		output, runErr := runArmEvaluationIsolated(ctx, cfg.WorkDir, arm, input, physicalEndpoint,
			cfg.APIKey, mainDB, mainPath, client)
		if runErr != nil {
			delta := httpAttemptDelta(beforeHTTP, observer.Snapshot())
			failedReport := observedArmReport(arm)
			applyHTTPObservation(&failedReport, delta, "document")
			switch arm {
			case ArmOldProduction:
				applyHTTPObservation(&failedReport, oldQueryHTTP, "query")
			case ArmNestedContext4:
				applyHTTPObservation(&failedReport, contextQueryHTTP, "query")
			}
			armReports[arm] = failedReport
			partial := newReport(cfg.RunID, cfg.Commit, corpusHash, corpus.QueryHash())
			partial.Complete = false
			partial.RunErrors = []string{fmt.Sprintf("evaluate %s: %v", arm, runErr)}
			partial.Arms = armReports
			partial.Exact = exactDiagnosticPolicy(cfg.ExactOracle, cfg.Distractors)
			partial.evaluateGates()
			return evaluationRun{Report: partial}, runErr
		}
		delta := httpAttemptDelta(beforeHTTP, observer.Snapshot())
		resetHTTPAccounting(&output.Report)
		applyHTTPObservation(&output.Report, delta, "document")
		switch arm {
		case ArmOldProduction:
			applyHTTPObservation(&output.Report, oldQueryHTTP, "query")
		case ArmNestedContext4:
			applyHTTPObservation(&output.Report, contextQueryHTTP, "query")
		}
		armReports[arm] = output.Report
		if output.Report.VectorNorms.P95 <= 0 {
			normErr := fmt.Errorf("%s persisted vector norm summary is empty", arm)
			partial := newReport(cfg.RunID, cfg.Commit, corpusHash, corpus.QueryHash())
			partial.Complete = false
			partial.RunErrors = []string{normErr.Error()}
			partial.Arms = armReports
			partial.SourceRows = sourceRows
			partial.FTSRows = ftsRows
			partial.Exact = exactDiagnosticPolicy(cfg.ExactOracle, cfg.Distractors)
			partial.evaluateGates()
			return evaluationRun{Report: partial}, normErr
		}
		rankings[arm] = output.Rankings
		winningProvenance[arm] = output.Provenance
		hybridRankings[arm] = make(map[string][]string, len(output.Results))
		for scenarioID, value := range output.Results {
			resultByID[scenarioID].ByArm[arm] = value
			hybridRankings[arm][scenarioID] = append([]string(nil), value.Hybrid...)
		}
	}

	report := newReport(cfg.RunID, cfg.Commit, corpusHash, corpus.QueryHash())
	report.SourceRows = sourceRows
	report.FTSRows = ftsRows
	report.Exact = exactDiagnosticPolicy(cfg.ExactOracle, cfg.Distractors)
	for _, arm := range arms {
		armReport := armReports[arm]
		armReport.Macro, armReport.ByFamily = aggregateArmMetrics(results, arm)
		if arm != ArmOldProduction {
			armReport.QueryCacheGroup = "context4-shared-across-O-c4-S-c4-N-c4"
		}
		report.Arms[arm] = armReport
	}
	report.Quality = qualityIntervals(results, cfg.Bootstrap)
	report.Operational = operationalSummary(results, report.Arms)
	report.Operational.ExactEvaluated = true
	report.evaluateGates()
	queries := make(map[string]string, len(corpus.Scenarios))
	for _, scenario := range corpus.Scenarios {
		queries[scenario.ID] = scenario.Query
	}
	byDocument := make(map[string][]string)
	for _, source := range sources {
		byDocument[source.DocumentID] = append(byDocument[source.DocumentID], source.StructuredChunks...)
	}
	poolSources := make(map[string]poolSource, len(sources))
	for _, source := range sources {
		excerpt := strings.Join(byDocument[source.DocumentID], "\n")
		if len(excerpt) > 4096 {
			excerpt = excerpt[:4096]
		}
		chunkID := source.ID
		if len(source.StructuredChunkIDs) > 0 {
			chunkID = source.StructuredChunkIDs[0]
		}
		poolSources[source.ID] = poolSource{Excerpt: excerpt,
			Provenance: chunkProvenance{Family: source.Family, Chunk: "document window"},
			MessageID:  source.MessageID, ChunkID: chunkID, DocumentID: source.DocumentID}
	}
	return evaluationRun{Report: report, Rankings: rankings, HybridRankings: hybridRankings,
		Queries: queries, Sources: poolSources, WinningProvenance: winningProvenance}, nil
}

func observedArmReport(arm string) ArmReport {
	config := evaluationVectorConfig(arm)
	return ArmReport{Model: config.Embeddings.Model, Config: armConfigReport(arm, config),
		RequestAccounting: "physical_http_attempts_including_retries",
		TokenAccounting:   "provider_response_usage_total_tokens"}
}

func resetHTTPAccounting(report *ArmReport) {
	report.Requests = 0
	report.DocumentRequests = 0
	report.QueryRequests = 0
	report.InputTokens = 0
	report.QueryInputTokens = 0
	report.SuccessfulResponses = 0
	report.UsageResponses = 0
	report.UsageAvailable = false
	report.Errors = 0
	report.HTTPStatuses = nil
	report.ErrorMessages = nil
	report.LatencyMillis = LatencySummary{}
	report.QueryLatencyMillis = LatencySummary{}
	report.DocumentLatencyMillis = LatencySummary{}
	report.latencySamples = nil
	report.RequestAccounting = "physical_http_attempts_including_retries"
	report.TokenAccounting = "provider_response_usage_total_tokens"
}

func applyHTTPObservation(report *ArmReport, delta HTTPAttemptSnapshot, phase string) {
	report.Requests += delta.Attempts
	report.SuccessfulResponses += delta.SuccessfulResponses
	report.UsageResponses += delta.UsageResponses
	report.InputTokens += delta.ResponseTokens
	report.Errors += delta.ErrorCount()
	if report.HTTPStatuses == nil {
		report.HTTPStatuses = make(map[int]int64)
	}
	for status, count := range delta.Statuses {
		report.HTTPStatuses[status] += count
	}
	report.ErrorMessages = append(report.ErrorMessages, delta.ErrorMessages...)
	report.latencySamples = append(report.latencySamples, delta.latencySamples...)
	report.LatencyMillis = summarizeLatencies(report.latencySamples)
	if phase == "query" {
		report.QueryRequests += delta.Attempts
		report.QueryInputTokens += delta.ResponseTokens
		report.QueryLatencyMillis = delta.LatencyMillis
	} else {
		report.DocumentRequests += delta.Attempts
		report.DocumentLatencyMillis = delta.LatencyMillis
	}
	report.UsageAvailable = report.SuccessfulResponses > 0 && report.UsageResponses == report.SuccessfulResponses && report.InputTokens > 0
	if delta.SuccessfulResponses > delta.UsageResponses {
		report.ErrorMessages = append(report.ErrorMessages, fmt.Sprintf("%s provider usage missing for %d successful responses",
			phase, delta.SuccessfulResponses-delta.UsageResponses))
	}
}

func partialEvaluationRun(cfg runConfig, corpusHash, queryHash string, arms map[string]ArmReport, message string) evaluationRun {
	report := newReport(cfg.RunID, cfg.Commit, corpusHash, queryHash)
	report.Complete = false
	report.RunErrors = []string{message}
	report.Arms = arms
	report.Exact = exactDiagnosticPolicy(cfg.ExactOracle, cfg.Distractors)
	report.evaluateGates()
	return evaluationRun{Report: report}
}

func scoreRanking(ann, exactL2, exactCosine []string, judgment Judgment) RankingMetrics {
	annGrades := gradesForRanking(ann, judgment)
	relevant := 0
	for _, grade := range judgment.Grades {
		if grade > 0 {
			relevant++
		}
	}
	return RankingMetrics{
		NDCGAt10: ndcgAt10(annGrades, gradeValues(judgment.Grades)), RecallAt10: recallAt(annGrades, 10, relevant),
		RecallAt20: recallAt(annGrades, 20, relevant), MRRAt10: reciprocalRankAt(annGrades, 10),
		HitAt5: hitAt(annGrades, 5), EvidenceHitAt10: evidenceHitAt(ann, judgment.EvidenceIDs, 10),
		ExactANNRecallAt10: topKOverlap(ann, exactL2, 10), L2CosineOverlap10: topKOverlap(exactL2, exactCosine, 10),
	}
}

func aggregateArmMetrics(results []scenarioResult, arm string) (RankingMetrics, map[string]RankingMetrics) {
	var all []RankingMetrics
	byFamilyValues := make(map[string][]RankingMetrics)
	for _, result := range results {
		value := result.ByArm[arm].Metrics
		all = append(all, value)
		byFamilyValues[result.Scenario.Family] = append(byFamilyValues[result.Scenario.Family], value)
	}
	byFamily := make(map[string]RankingMetrics, len(byFamilyValues))
	for family, values := range byFamilyValues {
		byFamily[family] = meanRankingMetrics(values)
	}
	return meanRankingMetrics(all), byFamily
}

func meanRankingMetrics(values []RankingMetrics) RankingMetrics {
	if len(values) == 0 {
		return RankingMetrics{}
	}
	var total RankingMetrics
	for _, value := range values {
		total.NDCGAt10 += value.NDCGAt10
		total.RecallAt10 += value.RecallAt10
		total.RecallAt20 += value.RecallAt20
		total.MRRAt10 += value.MRRAt10
		total.HitAt5 += value.HitAt5
		total.EvidenceHitAt10 += value.EvidenceHitAt10
		total.ExactANNRecallAt10 += value.ExactANNRecallAt10
		total.L2CosineOverlap10 += value.L2CosineOverlap10
	}
	count := float64(len(values))
	total.NDCGAt10 /= count
	total.RecallAt10 /= count
	total.RecallAt20 /= count
	total.MRRAt10 /= count
	total.HitAt5 /= count
	total.EvidenceHitAt10 /= count
	total.ExactANNRecallAt10 /= count
	total.L2CosineOverlap10 /= count
	return total
}

func qualityIntervals(results []scenarioResult, samples int) QualitySummary {
	contextual := scenarioPairs(results, ArmStructuredSingleton, "", false)
	endToEnd := scenarioPairs(results, ArmOldProduction, "", false)
	emailIndependent := scenarioPairs(results, ArmStructuredSingleton, "", true)
	hybridContext := hybridPairs(results, ArmStructuredSingleton, ArmNestedContext4, true)
	hybridOverall := hybridPairs(results, ArmStructuredSingleton, ArmNestedContext4, false)
	return QualitySummary{
		ContextualMacro: pairedBootstrap(contextual, samples, 550),
		ContextualByFamily: map[string]Interval{
			familyChat:       pairedBootstrap(scenarioPairs(results, ArmStructuredSingleton, familyChat, false), samples, 551),
			familyTranscript: pairedBootstrap(scenarioPairs(results, ArmStructuredSingleton, familyTranscript, false), samples, 552),
		},
		EndToEnd:           pairedBootstrap(endToEnd, samples, 553),
		EmailOrIndependent: pairedBootstrap(emailIndependent, samples, 554),
		HybridContextual:   pairedBootstrap(hybridContext, samples, 555),
		HybridOverall:      pairedBootstrap(hybridOverall, samples, 556),
	}
}

func scenarioPairs(results []scenarioResult, baseline, family string, emailOrIndependent bool) []ScenarioPair {
	var pairs []ScenarioPair
	for _, result := range results {
		if family != "" && result.Scenario.Family != family {
			continue
		}
		if emailOrIndependent && result.Scenario.Family != familyEmail && result.Scenario.ContextOnly {
			continue
		}
		pairs = append(pairs, ScenarioPair{ScenarioID: result.Scenario.ID, Family: result.Scenario.Family,
			Baseline: result.ByArm[baseline].Metrics.NDCGAt10, Candidate: result.ByArm[ArmNestedContext4].Metrics.NDCGAt10})
	}
	return pairs
}

func hybridPairs(results []scenarioResult, baseline, candidate string, contextOnly bool) []ScenarioPair {
	var pairs []ScenarioPair
	for _, result := range results {
		if contextOnly && !result.Scenario.ContextOnly {
			continue
		}
		judgmentBaseline := result.ByArm[baseline]
		judgmentCandidate := result.ByArm[candidate]
		pairs = append(pairs, ScenarioPair{ScenarioID: result.Scenario.ID, Family: result.Scenario.Family,
			Baseline: judgmentBaseline.HybridNDCG, Candidate: judgmentCandidate.HybridNDCG})
	}
	return pairs
}

func operationalSummary(results []scenarioResult, arms map[string]ArmReport) OperationalSummary {
	var recall, annOld, annNew, fullOld, fullNew []float64
	for _, result := range results {
		recall = append(recall, result.ByArm[ArmNestedContext4].Metrics.ExactANNRecallAt10)
		annOld = append(annOld, durationMillis(result.ByArm[ArmOldProduction].ANNTime))
		annNew = append(annNew, durationMillis(result.ByArm[ArmNestedContext4].ANNTime))
		fullOld = append(fullOld, durationMillis(result.ByArm[ArmOldProduction].FullTime))
		fullNew = append(fullNew, durationMillis(result.ByArm[ArmNestedContext4].FullTime))
	}
	oldArm, newArm := arms[ArmOldProduction], arms[ArmNestedContext4]
	var peakRSSRatio *float64
	if oldArm.Build.PeakRSSBytes != nil && newArm.Build.PeakRSSBytes != nil && *oldArm.Build.PeakRSSBytes > 0 {
		value := float64(*newArm.Build.PeakRSSBytes) / float64(*oldArm.Build.PeakRSSBytes)
		peakRSSRatio = &value
	}
	return OperationalSummary{
		ANNRecallAt10: mean(recall), CachedANNP95Ratio: safeRatio(percentile95(annNew), percentile95(annOld)),
		EndToEndP95Ratio:  safeRatio(percentile95(fullNew), percentile95(fullOld)),
		EndToEndP95Millis: percentile95(fullNew), RebuildTimeRatio: safeRatio(newArm.Build.WallMillis, oldArm.Build.WallMillis),
		PeakRSSRatio:            peakRSSRatio,
		IndexSizeRatio:          safeRatio(float64(newArm.Build.IndexBytes), float64(oldArm.Build.IndexBytes)),
		ProviderTokenRatio:      safeRatio(float64(newArm.InputTokens), float64(oldArm.InputTokens)),
		ProviderTokensAvailable: oldArm.UsageAvailable && newArm.UsageAvailable && oldArm.InputTokens > 0,
		RequestErrorRate:        safeRatio(float64(newArm.Errors), float64(max(newArm.Requests, 1))),
	}
}

func percentile95(values []float64) float64 {
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	return percentile(copyValues, 0.95)
}

func durationMillis(value time.Duration) float64 { return float64(value.Microseconds()) / 1000 }

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func safeRatio(candidate, baseline float64) float64 {
	if baseline == 0 {
		if candidate == 0 {
			return 1
		}
		return math.MaxFloat64
	}
	return candidate / baseline
}
