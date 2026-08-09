//go:build sqlite_vec

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/embed"
)

const (
	defaultScenarioPath = "testdata/contextual-eval/scenarios.jsonl"
	defaultJudgmentPath = "testdata/contextual-eval/judgments.jsonl"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "contextual-retrieval-eval:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: %s", strings.Join(commandNames(), ", "))
	}
	switch args[0] {
	case "generate":
		return runGenerate(ctx, args[1:])
	case "embed":
		return runEmbed(ctx, args[1:])
	case "pool":
		return runPool(args[1:])
	case "score":
		return runScore(ctx, args[1:])
	case "replay":
		return runReplay(ctx, args[1:])
	case "apply-replay":
		return runApplyReplay(args[1:])
	case "arm-run":
		return runArmEval(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

type armEvalInput struct {
	Documents []ArmDocument      `json:"documents"`
	Scenarios []armScenarioInput `json:"scenarios"`
	Exact     bool               `json:"exact"`
}

type armScenarioInput struct {
	Scenario  Scenario      `json:"scenario"`
	Judgment  Judgment      `json:"judgment"`
	Query     []float32     `json:"query_vector"`
	QueryTime time.Duration `json:"query_time"`
}

type armEvalOutput struct {
	Report     ArmReport                             `json:"report"`
	Rankings   map[string][]string                   `json:"rankings"`
	Results    map[string]rankingResult              `json:"results"`
	Provenance map[string]map[string][]chunkEvidence `json:"provenance"`
}

func runArmEval(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arm-run", flag.ContinueOnError)
	arm := flags.String("arm", "", "evaluation arm")
	inputPath := flags.String("input", "", "arm evaluation input JSON")
	mainPath := flags.String("main", "", "source database")
	dir := flags.String("dir", "", "arm output directory")
	endpoint := flags.String("endpoint", "", "metered embedding endpoint")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse arm-run flags: %w", err)
	}
	if err := validateArm(*arm); err != nil {
		return err
	}
	var input armEvalInput
	if err := readJSONFile(*inputPath, &input); err != nil {
		return err
	}
	mainStore, err := store.OpenForTest(*mainPath)
	if err != nil {
		return err
	}
	defer func() { _ = mainStore.Close() }()
	apiKey := os.Getenv("MSGVAULT_EVAL_CHILD_API_KEY")
	var client documentEmbedder
	if *arm == ArmOldProduction {
		client = embed.NewClient(embed.Config{Endpoint: *endpoint, APIKey: apiKey, Model: oldProductionModel,
			Dimension: evaluationDimension, Timeout: 60 * time.Second, MaxRetries: 3})
	} else {
		client = embed.NewVoyageClient(embed.VoyageConfig{Endpoint: *endpoint, APIKey: apiKey, Model: context4Model,
			Dimension: evaluationDimension, Timeout: 60 * time.Second, MaxRetries: 3})
	}
	index, report, err := buildFreshArmIndex(ctx, *dir, *arm, input.Documents, client, mainStore, *mainPath)
	if err != nil {
		return err
	}
	defer func() { _ = index.Close() }()
	output, err := evaluateArmIndex(ctx, index, report, input)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(output)
}

type commonFlags struct {
	scenarios   string
	judgments   string
	distractors int
	bootstrap   int
	output      string
	workDir     string
	endpoint    string
	scale       string
	exact       bool
}

func addCommonFlags(flags *flag.FlagSet) *commonFlags {
	values := &commonFlags{}
	flags.StringVar(&values.scenarios, "scenarios", defaultScenarioPath, "frozen scenarios JSONL")
	flags.StringVar(&values.judgments, "judgments", defaultJudgmentPath, "frozen judgments JSONL")
	flags.IntVar(&values.distractors, "distractors", 20000, "fixed-seed distractor count")
	flags.IntVar(&values.bootstrap, "bootstrap", 10000, "paired whole-scenario bootstrap samples")
	flags.StringVar(&values.output, "output", "", "machine-readable JSON output")
	flags.StringVar(&values.workDir, "work-dir", "", "retained evaluator work directory")
	flags.StringVar(&values.endpoint, "endpoint", "https://api.voyageai.com/v1", "Voyage API base endpoint")
	flags.StringVar(&values.scale, "scale", "", "comma-separated distractor scale sweep")
	flags.BoolVar(&values.exact, "exact-oracle", false, "emit exact ranking diagnostics")
	return values
}

func runGenerate(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	values := addCommonFlags(flags)
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse generate flags: %w", err)
	}
	corpus, err := loadCorpus(values.scenarios, values.judgments, values.distractors)
	if err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp("", "msgvault-contextual-generate-")
	if err != nil {
		return err
	}
	sources, _, _, cleanup, err := assembleStructuredCorpus(ctx, tempDir, corpus)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return err
	}
	defer cleanup()
	return writeJSON(values.output, map[string]any{
		"schema_version": 1, "corpus_hash": materializedCorpusHash(corpus, sources, evaluationPolicyFingerprint()),
		"query_hash": corpus.QueryHash(), "policy_fingerprint": evaluationPolicyFingerprint(),
		"families":    map[string]int{familyChat: corpus.CountFamily(familyChat), familyTranscript: corpus.CountFamily(familyTranscript), familyEmail: corpus.CountFamily(familyEmail)},
		"distractors": corpus.DistractorCount(), "seed": distractorSeed,
	})
}

func runEmbed(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("embed", flag.ContinueOnError)
	values := addCommonFlags(flags)
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse embed flags: %w", err)
	}
	if values.output == "" {
		return errors.New("embed requires --output")
	}
	run, err := executeEvaluation(ctx, values.runConfig())
	if err != nil {
		if run.Report.SchemaVersion != 0 {
			_ = writeJSON(values.output, run)
		}
		return err
	}
	return writeJSON(values.output, run)
}

func runScore(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("score", flag.ContinueOnError)
	values := addCommonFlags(flags)
	manifestPath := flags.String("manifest", "", "embed manifest JSON for blind scoring")
	privateKeyPath := flags.String("blind-key", "", "private blind handle map JSON")
	blindGradesPath := flags.String("blind-grades", "", "returned blind judgments JSON")
	replayReportPath := flags.String("replay-report", "", "authenticated replay result for append-token gate")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse score flags: %w", err)
	}
	if values.output == "" {
		return errors.New("score requires --output")
	}
	if *manifestPath != "" || *privateKeyPath != "" || *blindGradesPath != "" {
		if *manifestPath == "" || *privateKeyPath == "" || *blindGradesPath == "" {
			return errors.New("blind score requires --manifest, --blind-key, and --blind-grades")
		}
		report, err := scoreBlindFiles(*manifestPath, *privateKeyPath, *blindGradesPath)
		if err != nil {
			return err
		}
		return writeJSON(values.output, report)
	}
	if values.scale != "" {
		scales, err := parseScales(values.scale)
		if err != nil {
			return err
		}
		results := make(map[string]EvaluationReport, len(scales))
		baseWorkDir := values.workDir
		runID := time.Now().UTC().Format("20060102T150405.000000000Z")
		directories := scaleRunDirectories(baseWorkDir, runID, scales)
		for _, scale := range scales {
			values.distractors = scale
			if baseWorkDir != "" {
				values.workDir = directories[scale]
			}
			run, err := executeEvaluation(ctx, values.runConfig())
			if err != nil {
				return fmt.Errorf("scale %d: %w", scale, err)
			}
			if err := applyReplayReportFile(&run.Report, *replayReportPath); err != nil {
				return fmt.Errorf("scale %d replay report: %w", scale, err)
			}
			run.Report.evaluateGates()
			results[strconv.Itoa(scale)] = run.Report
		}
		return writeJSON(values.output, scaleSweepReport{SchemaVersion: 1, ScaleResults: results})
	}
	run, err := executeEvaluation(ctx, values.runConfig())
	if err != nil {
		if run.Report.SchemaVersion != 0 {
			_ = writeJSON(values.output, run.Report)
		}
		return err
	}
	if err := applyReplayReportFile(&run.Report, *replayReportPath); err != nil {
		return err
	}
	run.Report.evaluateGates()
	return writeJSON(values.output, run.Report)
}

func applyReplayReportFile(report *EvaluationReport, path string) error {
	if path == "" {
		return nil
	}
	var replay replayReport
	if err := readJSONFile(path, &replay); err != nil {
		return fmt.Errorf("read replay report: %w", err)
	}
	return applyReplayResult(report, replay)
}

func runApplyReplay(args []string) error {
	flags := flag.NewFlagSet("apply-replay", flag.ContinueOnError)
	reportPath := flags.String("report", "", "existing score report JSON")
	replayPath := flags.String("replay-report", "", "authenticated replay result")
	outputPath := flags.String("output", "", "recomposed score report JSON")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse apply-replay flags: %w", err)
	}
	if *reportPath == "" || *replayPath == "" || *outputPath == "" {
		return errors.New("apply-replay requires --report, --replay-report, and --output")
	}
	var report EvaluationReport
	if err := readJSONFile(*reportPath, &report); err != nil {
		return fmt.Errorf("read score report: %w", err)
	}
	if report.SchemaVersion != 1 || report.CorpusHash == "" {
		return fmt.Errorf("unsupported score report schema version %d", report.SchemaVersion)
	}
	if err := applyReplayReportFile(&report, *replayPath); err != nil {
		return err
	}
	report.evaluateGates()
	return writeJSON(*outputPath, report)
}

func applyReplayResult(report *EvaluationReport, replay replayReport) error {
	if replay.SchemaVersion != 2 {
		return fmt.Errorf("unsupported replay schema version %d", replay.SchemaVersion)
	}
	if replay.CorpusHash != report.CorpusHash {
		return fmt.Errorf("replay corpus hash %q does not match score corpus hash %q", replay.CorpusHash, report.CorpusHash)
	}
	if replay.TokenAccounting != "observed_provider_usage_total_tokens" {
		return fmt.Errorf("replay token accounting %q is not observed provider usage", replay.TokenAccounting)
	}
	if replay.Gate.Name != "append_tokens" || !replay.Gate.Evaluated {
		return errors.New("replay append-token gate is unavailable")
	}
	paths := []replayPathStats{replay.ProductionHistory.LegacyScheduled,
		replay.ProductionHistory.ContextualScheduled}
	for _, path := range paths {
		if !path.UsageAvailable || path.EmbeddedDocuments <= 0 || path.ProviderTokens <= 0 || path.ProviderRequests <= 0 ||
			path.ProviderErrors != 0 || path.ProviderSuccessfulResponses != path.ProviderRequests ||
			path.ProviderUsageResponses != path.ProviderSuccessfulResponses {
			return errors.New("replay provider token usage is incomplete")
		}
	}
	ratio := safeRatio(float64(paths[1].ProviderTokens), float64(paths[0].ProviderTokens))
	if replay.Gate.Value != ratio || replay.Gate.Passed != (ratio <= 5) {
		return errors.New("replay append-token gate does not match observed provider usage")
	}
	report.Operational.AppendTokensAvailable = true
	report.Operational.AppendTokenAmplification = ratio
	return nil
}

type blindScoreReport struct {
	SchemaVersion  int                                  `json:"schema_version"`
	JudgmentSource string                               `json:"judgment_source"`
	ByArm          map[string]map[string]RankingMetrics `json:"by_arm"`
}

func scoreBlindFiles(manifestPath, privatePath, gradesPath string) (blindScoreReport, error) {
	var run evaluationRun
	if err := readJSONFile(manifestPath, &run); err != nil {
		return blindScoreReport{}, err
	}
	var private blindPrivateMap
	if err := readJSONFile(privatePath, &private); err != nil {
		return blindScoreReport{}, err
	}
	var grades blindGrades
	if err := readJSONFile(gradesPath, &grades); err != nil {
		return blindScoreReport{}, err
	}
	if run.Report.CorpusHash != private.CorpusHash || run.Report.QueryHash != private.QueryHash {
		return blindScoreReport{}, errors.New("blind key hashes do not match evaluation manifest")
	}
	if err := validateBlindBundle(run, private); err != nil {
		return blindScoreReport{}, err
	}
	judgments, err := unblindJudgments(grades, private)
	if err != nil {
		return blindScoreReport{}, err
	}
	report := blindScoreReport{SchemaVersion: 1, JudgmentSource: "returned_blind_judgments",
		ByArm: make(map[string]map[string]RankingMetrics)}
	for arm, scenarios := range run.Rankings {
		report.ByArm[arm] = make(map[string]RankingMetrics)
		for scenarioID, ranking := range scenarios {
			judgment, ok := judgments[scenarioID]
			if !ok {
				return blindScoreReport{}, fmt.Errorf("blind judgments missing scenario %q", scenarioID)
			}
			report.ByArm[arm][scenarioID] = scoreRanking(ranking, nil, nil, judgment)
		}
	}
	return report, nil
}

func readJSONFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return json.NewDecoder(file).Decode(target)
}

func scaleRunDirectories(base, runID string, scales []int) map[int]string {
	result := make(map[int]string, len(scales))
	for _, scale := range scales {
		result[scale] = filepath.Join(base, "run-"+runID, "scale-"+strconv.Itoa(scale))
	}
	return result
}

func (f commonFlags) runConfig() runConfig {
	return runConfig{
		ScenarioPath: f.scenarios, JudgmentPath: f.judgments, Distractors: f.distractors,
		Bootstrap: f.bootstrap, APIKey: os.Getenv("VOYAGE_API_KEY"), Endpoint: f.endpoint,
		WorkDir: f.workDir, RunID: time.Now().UTC().Format("20060102T150405Z"), Commit: repositoryCommit(), ExactOracle: f.exact,
	}
}

func parseScales(value string) ([]int, error) {
	var scales []int
	seen := make(map[int]struct{})
	for item := range strings.SplitSeq(value, ",") {
		parsed, err := strconv.Atoi(strings.TrimSpace(item))
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid scale %q", item)
		}
		if _, exists := seen[parsed]; !exists {
			scales = append(scales, parsed)
			seen[parsed] = struct{}{}
		}
	}
	return scales, nil
}

type pooledScenario struct {
	ScenarioID string            `json:"scenario_id"`
	Query      string            `json:"query"`
	Candidates []pooledCandidate `json:"candidates"`
}

type pooledCandidate struct {
	Handle     string          `json:"handle"`
	Excerpt    string          `json:"excerpt"`
	Provenance chunkProvenance `json:"provenance"`
}

type chunkProvenance struct {
	Family string `json:"family"`
	Chunk  string `json:"chunk"`
}

type blindPublicArtifact struct {
	SchemaVersion int              `json:"schema_version"`
	Blind         bool             `json:"blind"`
	CorpusHash    string           `json:"corpus_hash"`
	QueryHash     string           `json:"query_hash"`
	PoolHash      string           `json:"pool_hash"`
	Pool          []pooledScenario `json:"pool"`
}

type blindPrivateMap struct {
	SchemaVersion int                         `json:"schema_version"`
	CorpusHash    string                      `json:"corpus_hash"`
	QueryHash     string                      `json:"query_hash"`
	PoolHash      string                      `json:"pool_hash"`
	Handles       map[string]privateCandidate `json:"handles"`
}

type privateCandidate struct {
	ScenarioID string         `json:"scenario_id"`
	SourceID   string         `json:"source_id"`
	MessageID  int64          `json:"message_id"`
	ChunkID    string         `json:"chunk_id"`
	DocumentID string         `json:"document_id"`
	Wins       []candidateWin `json:"wins,omitempty"`
}

type candidateWin struct {
	Arm        string `json:"arm"`
	Ranker     string `json:"ranker"`
	Rank       int    `json:"rank"`
	MessageID  int64  `json:"message_id"`
	ChunkID    string `json:"chunk_id"`
	ChunkIndex int    `json:"chunk_index"`
	DocumentID string `json:"document_id"`
	RawStart   int    `json:"raw_start"`
	RawEnd     int    `json:"raw_end"`
}

type blindGrades struct {
	SchemaVersion int                     `json:"schema_version"`
	CorpusHash    string                  `json:"corpus_hash"`
	QueryHash     string                  `json:"query_hash"`
	PoolHash      string                  `json:"pool_hash"`
	Judgments     []blindScenarioJudgment `json:"judgments"`
}

type blindScenarioJudgment struct {
	ScenarioID      string         `json:"scenario_id"`
	Grades          map[string]int `json:"grades"`
	EvidenceHandles []string       `json:"evidence_handles,omitempty"`
}

type scaleSweepReport struct {
	SchemaVersion int                         `json:"schema_version"`
	ScaleResults  map[string]EvaluationReport `json:"scale_results"`
}

func runPool(args []string) error {
	flags := flag.NewFlagSet("pool", flag.ContinueOnError)
	input := flags.String("input", "", "embed manifest JSON")
	output := flags.String("output", "", "blind pool JSON")
	key := flags.String("key", "", "private handle map JSON")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse pool flags: %w", err)
	}
	if *input == "" || *output == "" || *key == "" {
		return errors.New("pool requires --input, --output, and --key")
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var run evaluationRun
	if err := json.NewDecoder(file).Decode(&run); err != nil {
		return err
	}
	public, private, err := buildBlindBundle(run)
	if err != nil {
		return err
	}
	if err := writeJSON(*key, private); err != nil {
		return err
	}
	return writeJSON(*output, public)
}

func buildBlindBundle(run evaluationRun) (blindPublicArtifact, blindPrivateMap, error) {
	pool, err := buildBlindPool(run)
	if err != nil {
		return blindPublicArtifact{}, blindPrivateMap{}, err
	}
	poolHash, err := blindPoolHash(run.Report.CorpusHash, run.Report.QueryHash, pool)
	if err != nil {
		return blindPublicArtifact{}, blindPrivateMap{}, err
	}
	private := blindPrivateMap{SchemaVersion: 1, CorpusHash: run.Report.CorpusHash,
		QueryHash: run.Report.QueryHash, PoolHash: poolHash, Handles: make(map[string]privateCandidate)}
	for scenarioID, sources := range pooledSources(run) {
		for sourceID := range sources {
			handle := opaqueCandidateHandle(scenarioID, sourceID)
			source := run.Sources[sourceID]
			candidate := privateCandidate{ScenarioID: scenarioID, SourceID: sourceID,
				MessageID: source.MessageID, ChunkID: source.ChunkID, DocumentID: source.DocumentID,
				Wins: winningCandidateProvenance(run, scenarioID, sourceID)}
			if len(candidate.Wins) > 0 {
				candidate.MessageID = candidate.Wins[0].MessageID
				candidate.ChunkID = candidate.Wins[0].ChunkID
				candidate.DocumentID = candidate.Wins[0].DocumentID
			}
			private.Handles[handle] = candidate
		}
	}
	return blindPublicArtifact{SchemaVersion: 1, Blind: true, CorpusHash: run.Report.CorpusHash,
		QueryHash: run.Report.QueryHash, PoolHash: poolHash, Pool: pool}, private, nil
}

func blindPoolHash(corpusHash, queryHash string, pool []pooledScenario) (string, error) {
	payload, err := json.Marshal(struct {
		CorpusHash string           `json:"corpus_hash"`
		QueryHash  string           `json:"query_hash"`
		Pool       []pooledScenario `json:"pool"`
	}{corpusHash, queryHash, pool})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func winningCandidateProvenance(run evaluationRun, scenarioID, sourceID string) []candidateWin {
	var result []candidateWin
	for arm, scenarios := range run.WinningProvenance {
		for ranker, winners := range scenarios[scenarioID] {
			for rank, winner := range winners[:min(20, len(winners))] {
				if winner.SourceID != sourceID {
					continue
				}
				result = append(result, candidateWin{Arm: arm, Ranker: ranker, Rank: rank + 1,
					MessageID: winner.MessageID, ChunkID: winner.ID, ChunkIndex: winner.ChunkIndex,
					DocumentID: winner.DocumentID, RawStart: winner.Start, RawEnd: winner.End})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Arm != result[j].Arm {
			return result[i].Arm < result[j].Arm
		}
		if result[i].Ranker != result[j].Ranker {
			return result[i].Ranker < result[j].Ranker
		}
		return result[i].Rank < result[j].Rank
	})
	return result
}

func validateBlindBundle(run evaluationRun, private blindPrivateMap) error {
	_, expected, err := buildBlindBundle(run)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, private) {
		return errors.New("blind key does not match evaluation manifest pooled union and provenance")
	}
	return nil
}

func pooledSources(run evaluationRun) map[string]map[string]struct{} {
	byScenario := make(map[string]map[string]struct{})
	for _, allRankings := range []map[string]map[string][]string{run.Rankings, run.HybridRankings} {
		for _, scenarios := range allRankings {
			for scenarioID, ranking := range scenarios {
				if byScenario[scenarioID] == nil {
					byScenario[scenarioID] = make(map[string]struct{})
				}
				for _, id := range ranking[:min(20, len(ranking))] {
					byScenario[scenarioID][id] = struct{}{}
				}
			}
		}
	}
	return byScenario
}

func buildBlindPool(run evaluationRun) ([]pooledScenario, error) {
	byScenario := pooledSources(run)
	pool := make([]pooledScenario, 0, len(byScenario))
	for scenarioID, candidates := range byScenario {
		items := make([]pooledCandidate, 0, len(candidates))
		for id := range candidates {
			source, ok := run.Sources[id]
			if !ok {
				return nil, fmt.Errorf("pool source %q is missing", id)
			}
			items = append(items, pooledCandidate{Handle: opaqueCandidateHandle(scenarioID, id), Excerpt: source.Excerpt,
				Provenance: source.Provenance})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Handle < items[j].Handle })
		pool = append(pool, pooledScenario{ScenarioID: scenarioID, Query: run.Queries[scenarioID], Candidates: items})
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].ScenarioID < pool[j].ScenarioID })
	return pool, nil
}

func unblindJudgments(grades blindGrades, private blindPrivateMap) (map[string]Judgment, error) {
	if grades.CorpusHash != private.CorpusHash {
		return nil, fmt.Errorf("blind corpus hash mismatch: grades %q key %q", grades.CorpusHash, private.CorpusHash)
	}
	if grades.QueryHash != private.QueryHash {
		return nil, fmt.Errorf("blind query hash mismatch: grades %q key %q", grades.QueryHash, private.QueryHash)
	}
	if grades.PoolHash != private.PoolHash {
		return nil, fmt.Errorf("blind pool hash mismatch: grades %q key %q", grades.PoolHash, private.PoolHash)
	}
	result := make(map[string]Judgment, len(grades.Judgments))
	for _, item := range grades.Judgments {
		if _, exists := result[item.ScenarioID]; exists {
			return nil, fmt.Errorf("duplicate blind judgment scenario %q", item.ScenarioID)
		}
		judgment := Judgment{ScenarioID: item.ScenarioID, Grades: make(map[string]int)}
		for handle, grade := range item.Grades {
			if grade < 0 || grade > 3 {
				return nil, fmt.Errorf("blind grade for %q is outside 0..3", handle)
			}
			mapped, ok := private.Handles[handle]
			if !ok || mapped.ScenarioID != item.ScenarioID {
				return nil, fmt.Errorf("unknown blind handle %q for scenario %q", handle, item.ScenarioID)
			}
			judgment.Grades[mapped.SourceID] = grade
		}
		seenEvidence := make(map[string]struct{}, len(item.EvidenceHandles))
		for _, handle := range item.EvidenceHandles {
			if _, exists := seenEvidence[handle]; exists {
				return nil, fmt.Errorf("duplicate evidence handle %q", handle)
			}
			seenEvidence[handle] = struct{}{}
			mapped, ok := private.Handles[handle]
			if !ok || mapped.ScenarioID != item.ScenarioID {
				return nil, fmt.Errorf("unknown evidence handle %q for scenario %q", handle, item.ScenarioID)
			}
			judgment.EvidenceIDs = append(judgment.EvidenceIDs, mapped.SourceID)
		}
		result[item.ScenarioID] = judgment
	}
	for handle, mapped := range private.Handles {
		judgment, ok := result[mapped.ScenarioID]
		if !ok {
			return nil, fmt.Errorf("blind judgments missing scenario %q", mapped.ScenarioID)
		}
		if _, ok := judgment.Grades[mapped.SourceID]; !ok {
			return nil, fmt.Errorf("blind judgment missing pooled handle %q", handle)
		}
	}
	return result, nil
}

func opaqueCandidateHandle(scenarioID, sourceID string) string {
	digest := sha256.Sum256([]byte(scenarioID + "\x00" + sourceID))
	return fmt.Sprintf("candidate-%x", digest[:8])
}

func writeJSON(path string, value any) error {
	if path == "" || path == "-" {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	committed = true
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func repositoryCommit() string {
	output, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

type replayMode struct {
	Tokens        int64   `json:"tokens"`
	Amplification float64 `json:"amplification"`
	Replacement   string  `json:"replacement_policy"`
	Publications  int     `json:"production_publications"`
	Replacements  int     `json:"production_replacements"`
	Available     bool    `json:"available"`
	Error         string  `json:"error,omitempty"`
}

type replayDay struct {
	Day            int `json:"day"`
	Appends        int `json:"appends"`
	ScheduledTicks int `json:"scheduled_ticks"`
}

type replayReport struct {
	SchemaVersion     int                     `json:"schema_version"`
	Days              int                     `json:"days"`
	CorpusHash        string                  `json:"corpus_hash"`
	BaselineTokens    int64                   `json:"baseline_tokens"`
	Workload          replayWorkload          `json:"workload"`
	Modes             map[string]replayMode   `json:"modes"`
	Chronology        []replayDay             `json:"chronology"`
	Gate              GateResult              `json:"gate"`
	ProductionHistory productionReplayHistory `json:"production_history"`
	TokenAccounting   string                  `json:"token_accounting"`
}

func runReplay(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	days := flags.Int("days", 7, "chronological append days")
	output := flags.String("output", "", "machine-readable replay report")
	scenarios := flags.String("scenarios", defaultScenarioPath, "frozen scenarios JSONL")
	judgments := flags.String("judgments", defaultJudgmentPath, "frozen judgments JSONL")
	distractors := flags.Int("distractors", 20000, "fixed-seed distractor count")
	endpoint := flags.String("endpoint", "https://api.voyageai.com/v1", "Voyage API base endpoint")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse replay flags: %w", err)
	}
	if *days <= 0 || *output == "" || *distractors < 0 {
		return errors.New("replay requires positive --days and --output")
	}
	apiKey := os.Getenv("VOYAGE_API_KEY")
	if apiKey == "" {
		return errors.New("replay requires VOYAGE_API_KEY for observed provider usage")
	}
	corpus, err := loadCorpus(*scenarios, *judgments, *distractors)
	if err != nil {
		return err
	}
	events := buildReplayEvents(*days)
	proxyHandler, observer, proxyErr := newCountingEmbeddingProxy(*endpoint)
	if proxyErr != nil {
		return proxyErr
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()
	legacy := embed.NewClient(embed.Config{Endpoint: proxy.URL, APIKey: apiKey, Model: oldProductionModel,
		Dimension: evaluationDimension, Timeout: 60 * time.Second, MaxRetries: 3})
	scheduled := embed.NewVoyageClient(embed.VoyageConfig{Endpoint: proxy.URL, APIKey: apiKey, Model: context4Model,
		Dimension: evaluationDimension, Timeout: 60 * time.Second, MaxRetries: 3})
	stress := embed.NewVoyageClient(embed.VoyageConfig{Endpoint: proxy.URL, APIKey: apiKey, Model: context4Model,
		Dimension: evaluationDimension, Timeout: 60 * time.Second, MaxRetries: 3})
	history, err := replayProductionHistoryWithClients(ctx, os.TempDir(), events, replayClients{
		LegacyScheduled: legacy, ContextualScheduled: scheduled, ContextualPerAppendStress: stress, Observer: observer})
	if err != nil {
		return fmt.Errorf("run production replay history: %w", err)
	}
	tempDir, err := os.MkdirTemp("", "msgvault-contextual-replay-")
	if err != nil {
		return err
	}
	sources, _, _, cleanup, err := assembleStructuredCorpus(ctx, tempDir, corpus)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return err
	}
	defer cleanup()
	corpusHash := materializedCorpusHash(corpus, sources, evaluationPolicyFingerprint())
	daily := make([]replayDay, *days)
	for day := range daily {
		daily[day].Day = day + 1
	}
	for _, event := range events {
		switch event.Kind {
		case replayMutationEvent:
			daily[event.Day-1].Appends++
		case replayTickEvent:
			daily[event.Day-1].ScheduledTicks++
		}
	}
	observedBaseline := history.LegacyScheduled.ProviderTokens
	observedScheduled := history.ContextualScheduled.ProviderTokens
	observedStress := history.ContextualPerAppendStress.ProviderTokens
	usageAvailable := history.LegacyScheduled.UsageAvailable && history.ContextualScheduled.UsageAvailable &&
		observedBaseline > 0
	scheduledRatio := safeRatio(float64(observedScheduled), float64(observedBaseline))
	report := replayReport{
		SchemaVersion: 2, Days: *days, CorpusHash: corpusHash, BaselineTokens: observedBaseline,
		Workload: summarizeReplayWorkload(events),
		Modes: map[string]replayMode{
			"legacy_scheduled": {Tokens: observedBaseline, Amplification: 1, Replacement: "embed all committed messages once at each successful sync or scheduled tick",
				Publications: history.LegacyScheduled.Publications, Replacements: history.LegacyScheduled.Replacements,
				Available: history.LegacyScheduled.UsageAvailable},
			"contextual_scheduled": {Tokens: observedScheduled, Amplification: scheduledRatio, Replacement: "publish the latest complete source snapshot once at each successful sync or scheduled tick",
				Publications: history.ContextualScheduled.Publications, Replacements: history.ContextualScheduled.Replacements,
				Available: history.ContextualScheduled.UsageAvailable},
			"contextual_per_append_stress": {Tokens: observedStress, Amplification: safeRatio(float64(observedStress), float64(observedBaseline)), Replacement: "stress diagnostic: publish after every committed message row",
				Publications: history.ContextualPerAppendStress.Publications, Replacements: history.ContextualPerAppendStress.Replacements,
				Available: history.ContextualPerAppendStress.UsageAvailable, Error: history.ContextualPerAppendStress.Error},
		},
		Chronology:        daily,
		Gate:              GateResult{Name: "append_tokens", Evaluated: usageAvailable, Passed: usageAvailable && scheduledRatio <= 5, Value: scheduledRatio, Limit: "<=5"},
		ProductionHistory: history,
		TokenAccounting:   "observed_provider_usage_total_tokens",
	}
	return writeJSON(*output, report)
}
