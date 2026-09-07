//go:build sqlite_vec

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestCausalIsolation_ChatKeepsEvidenceOutOfAnswerSingleton(t *testing.T) {
	scenario := Scenario{ID: "chat-001", Family: familyChat, Query: "cobalt launch decision", ContextOnly: true,
		PositiveID: "chat-001-doc-a", HardNegativeID: "chat-001-doc-b"}
	documents, cleanup, err := assembleScenarioDocuments(t.Context(), t.TempDir(), scenario)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	positive := sourceByExternalID(t, documents, scenario.PositiveID)
	negative := sourceByExternalID(t, documents, scenario.HardNegativeID)
	assert.Equal(t, positive.OldChunks, negative.OldChunks)
	assert.Equal(t, positive.StructuredChunks, negative.StructuredChunks,
		"the S-c4 answer singletons must be byte-identical")
	assert.NotContains(t, positive.StructuredChunkText(scenario.PositiveID), scenario.Query)
	assert.NotContains(t, strings.Join(negative.StructuredChunks, "\n"), scenario.Query)

	positiveEvidence := sourceByExternalID(t, documents, scenario.PositiveID+"-evidence")
	negativeEvidence := sourceByExternalID(t, documents, scenario.HardNegativeID+"-evidence")
	assert.Contains(t, strings.Join(positiveEvidence.StructuredChunks, "\n"), scenario.Query)
	assert.NotContains(t, strings.Join(negativeEvidence.StructuredChunks, "\n"), scenario.Query)
	assert.Equal(t, positive.DocumentID, positiveEvidence.DocumentID)
	assert.Equal(t, negative.DocumentID, negativeEvidence.DocumentID)

	singletons := buildArmDocuments(ArmStructuredSingleton, documents)
	nested := buildArmDocuments(ArmNestedContext4, documents)
	assertSameChunkBytesDifferentGrouping(t, singletons, nested)
}

func TestCausalIsolation_TranscriptUsesChunkEvidenceWithoutMessageCollapse(t *testing.T) {
	scenario := Scenario{ID: "transcript-001", Family: familyTranscript, Query: "cobalt launch decision", ContextOnly: true,
		PositiveID: "transcript-001-doc-a", HardNegativeID: "transcript-001-doc-b"}
	documents, cleanup, err := assembleScenarioDocuments(t.Context(), t.TempDir(), scenario)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	positive := sourceByExternalID(t, documents, scenario.PositiveID)
	negative := sourceByExternalID(t, documents, scenario.HardNegativeID)
	assert.Equal(t, positive.StructuredChunkText(scenario.PositiveID+"-evidence"),
		negative.StructuredChunkText(scenario.HardNegativeID+"-evidence"))
	assert.NotEmpty(t, positive.StructuredChunkIDs)
	assert.Contains(t, positive.StructuredChunkIDs, scenario.PositiveID+"-evidence")
	assert.NotContains(t, positive.StructuredChunkText(scenario.PositiveID+"-evidence"), scenario.Query)
	assert.Contains(t, positive.StructuredChunkText(scenario.PositiveID+"-context"), scenario.Query)

	singletons := buildArmDocuments(ArmStructuredSingleton, documents)
	nested := buildArmDocuments(ArmNestedContext4, documents)
	assertSameChunkBytesDifferentGrouping(t, singletons, nested)
	judgment := Judgment{Grades: map[string]int{scenario.PositiveID: 3}, EvidenceIDs: []string{scenario.PositiveID + "-evidence"}}
	assert.InDelta(t, 0.0, scoreRanking([]string{scenario.PositiveID}, nil, nil, judgment).EvidenceHitAt10, 0.0001)
	assert.InDelta(t, 1.0, scoreRanking([]string{scenario.PositiveID + "-evidence"}, nil, nil, judgment).EvidenceHitAt10, 0.0001)
}

func sourceByExternalID(t *testing.T, documents []SourceDocument, id string) SourceDocument {
	t.Helper()
	for _, document := range documents {
		if document.ID == id {
			return document
		}
	}
	require.FailNow(t, "missing source document", id)
	return SourceDocument{}
}

func assertSameChunkBytesDifferentGrouping(t *testing.T, singletons, nested []ArmDocument) {
	t.Helper()
	want := flattenChunkText(singletons)
	got := flattenChunkText(nested)
	sort.Strings(want)
	sort.Strings(got)
	assert.Equal(t, want, got)
	assert.Greater(t, len(singletons), len(nested))
	for _, document := range singletons {
		assert.Len(t, document.Chunks, 1)
	}
}

func fixturePaths() (string, string) {
	root := filepath.Join("..", "..", "testdata", "contextual-eval")
	return filepath.Join(root, "scenarios.jsonl"), filepath.Join(root, "judgments.jsonl")
}

func TestNDCGAt10_KnownRanking(t *testing.T) {
	grades := []int{3, 0, 2, 1}
	assert.InDelta(t, 0.9508, ndcgAt10(grades, grades), 0.0001)
}

func TestNDCGAt10_PenalizesRelevantDocumentsMissingFromRanking(t *testing.T) {
	retrieved := []int{3, 2, 1}
	allJudged := []int{3, 3, 3, 3, 3, 2, 1, 0}
	assert.Less(t, ndcgAt10(retrieved, allJudged), 0.6)
}

func TestRankingMetrics_KnownResults(t *testing.T) {
	grades := []int{0, 3, 0, 1}
	assert.InDelta(t, 1, recallAt(grades, 10, 2), 0.0001)
	assert.InDelta(t, 0.5, reciprocalRankAt(grades, 10), 0.0001)
	assert.InDelta(t, 1.0, hitAt(grades, 5), 0.0001)
	assert.InDelta(t, 0.0, hitAt([]int{0, 0}, 5), 0.0001)
}

func TestCorpus_HasRequiredStrataAndNoRealisticPII(t *testing.T) {
	scenarios, judgments := fixturePaths()
	corpus, err := loadCorpus(scenarios, judgments, 20000)
	require.NoError(t, err)
	assert.Equal(t, 80, corpus.CountFamily(familyChat))
	assert.Equal(t, 80, corpus.CountFamily(familyTranscript))
	assert.Equal(t, 40, corpus.CountFamily(familyEmail))
	assert.GreaterOrEqual(t, corpus.DistractorCount(), 20000)
	assert.GreaterOrEqual(t, corpus.ContextOnlyCount(familyChat), 40)
	assert.GreaterOrEqual(t, corpus.ContextOnlyCount(familyTranscript), 40)
	require.NoError(t, corpus.Validate())
	assert.Equal(t, "e94159010f95e800b3aa404e2e74272d43757d9f5dd54d21b5a658c6e6e669e3", corpus.Hash())
}

func TestCorpus_DistractorsAreFixedSeedDeterministic(t *testing.T) {
	scenarios, judgments := fixturePaths()
	first, err := loadCorpus(scenarios, judgments, 20000)
	require.NoError(t, err)
	second, err := loadCorpus(scenarios, judgments, 20000)
	require.NoError(t, err)
	require.Len(t, first.Distractors, 20000)
	assert.Equal(t, first.Distractors, second.Distractors)
	assert.Equal(t, "distractor-00000", first.Distractors[0].ID)
	assert.Equal(t, "distractor-19999", first.Distractors[19999].ID)
}

func TestFourArms_KeepStructuredChunkBytesIdentical(t *testing.T) {
	scenario := Scenario{ID: "chat-001", Family: familyChat, Query: "cobalt launch decision", ContextOnly: true,
		PositiveID: "chat-001-positive", HardNegativeID: "chat-001-negative"}
	documents, cleanup, err := assembleScenarioDocuments(t.Context(), t.TempDir(), scenario)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	structuredSingleton := buildArmDocuments(ArmStructuredSingleton, documents)
	nested := buildArmDocuments(ArmNestedContext4, documents)
	require.NotEmpty(t, structuredSingleton)
	require.NotEmpty(t, nested)
	singletonText, nestedText := flattenChunkText(structuredSingleton), flattenChunkText(nested)
	sort.Strings(singletonText)
	sort.Strings(nestedText)
	assert.Equal(t, singletonText, nestedText)
	for _, document := range structuredSingleton {
		assert.Len(t, document.Chunks, 1)
	}
	assert.Less(t, len(nested), len(structuredSingleton))
}

func TestContextOnlyHardNegative_MatchesLocalTextButNotSurroundingEvidence(t *testing.T) {
	scenario := Scenario{ID: "chat-001", Family: familyChat, Query: "cobalt launch decision", ContextOnly: true,
		PositiveID: "chat-001-positive", HardNegativeID: "chat-001-negative"}
	documents, cleanup, err := assembleScenarioDocuments(t.Context(), t.TempDir(), scenario)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	byID := make(map[string]SourceDocument)
	for _, document := range documents {
		byID[document.ID] = document
	}
	positive := byID[scenario.PositiveID]
	negative := byID[scenario.HardNegativeID]
	assert.Equal(t, positive.OldChunks, negative.OldChunks)
	assert.Equal(t, positive.StructuredChunks, negative.StructuredChunks)
	var positiveContext, negativeContext []string
	for id, document := range byID {
		if strings.HasPrefix(id, scenario.PositiveID) {
			positiveContext = append(positiveContext, document.StructuredChunks...)
		}
		if strings.HasPrefix(id, scenario.HardNegativeID) {
			negativeContext = append(negativeContext, document.StructuredChunks...)
		}
	}
	assert.Contains(t, strings.Join(positiveContext, "\n"), scenario.Query)
	assert.NotContains(t, strings.Join(negativeContext, "\n"), scenario.Query)
}

func TestContext4Arms_ShareQueryCache(t *testing.T) {
	client := &countingEmbedder{queryVector: []float32{1, 0}}
	cache := newQueryCache(client)
	for _, arm := range []string{ArmOldContext4Singleton, ArmStructuredSingleton, ArmNestedContext4} {
		vector, err := cache.ForArm(t.Context(), arm, "synthetic query")
		require.NoError(t, err)
		assert.Equal(t, []float32{1, 0}, vector)
	}
	assert.Equal(t, 1, client.queryCalls)
}

func TestEmbedDocumentBatches_BoundsRequestsAndPreservesOrder(t *testing.T) {
	inputs := make([]embed.DocumentInput, 65)
	for i := range inputs {
		inputs[i] = embed.DocumentInput{Chunks: []string{fmt.Sprintf("document-%02d", i)}}
	}
	client := &batchRecordingEmbedder{}
	vectors, _, err := embedDocumentBatches(t.Context(), inputs, 32, client)
	require.NoError(t, err)
	assert.Equal(t, []int{32, 32, 1}, client.batchSizes)
	require.Len(t, vectors, 65)
	assert.InDelta(t, float32(0), vectors[0][0][0], 0.0001)
	assert.InDelta(t, float32(64), vectors[64][0][0], 0.0001)
}

func TestStreamEmbedArmDocuments_KeepsOneMessageAtomicAcrossBatchBoundary(t *testing.T) {
	corpus := Corpus{Scenarios: []Scenario{{ID: "email-001", Family: familyEmail, Query: "synthetic query",
		PositiveID: "email-001-positive", HardNegativeID: "email-001-negative"}},
		Judgments: map[string]Judgment{"email-001": {ScenarioID: "email-001", Grades: map[string]int{"email-001-positive": 3}}}}
	sources, mainStore, mainPath, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.NoError(t, sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"),
		MainDB: mainStore.DB(), MainPath: mainPath, Dimension: evaluationDimension})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	generation, err := backend.CreateGeneration(t.Context(), "synthetic", evaluationDimension, "synthetic")
	require.NoError(t, err)
	messageID := sources[0].MessageID
	documents := []ArmDocument{
		{Key: "first", Chunks: []EvalChunk{{MessageID: messageID, ChunkIndex: 0, Text: "first chunk"}}},
		{Key: "second", Chunks: []EvalChunk{{MessageID: messageID, ChunkIndex: 1, Text: "second chunk"}}},
	}
	client := &batchRecordingEmbedder{dimension: evaluationDimension}
	_, err = streamEmbedArmDocuments(t.Context(), generation, backend, documents, 1, client)
	require.NoError(t, err)
	assert.Equal(t, []int{2}, client.batchSizes)
	var rows int
	require.NoError(t, backend.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM embeddings WHERE generation_id = ? AND message_id = ?`, int64(generation), messageID).Scan(&rows))
	assert.Equal(t, 2, rows)
}

func TestRunContextWorkerUntilConverged_RequiresProgress(t *testing.T) {
	worker := &scriptedContextWorker{results: []embed.RunResult{
		{Claimed: 64, Succeeded: 64, Contextual: &embed.ContextConvergence{}},
		{Claimed: 36, Succeeded: 36, Contextual: &embed.ContextConvergence{Converged: true}},
	}}
	total, err := runContextWorkerUntilConverged(
		t.Context(), worker, vector.GenerationID(1), "contextual-eval:test",
	)
	require.NoError(t, err)
	assert.Equal(t, 100, total.Claimed)
	assert.Equal(t, 100, total.Succeeded)
	assert.True(t, total.Contextual.Converged)
	require.Len(t, worker.scopes, 2)
	assert.Equal(t, "contextual-eval:test:pass:1", worker.scopes[0].Key)
	assert.Equal(t, "contextual-eval:test:pass:2", worker.scopes[1].Key)
	assert.Equal(t, operations.TriggerManual, worker.scopes[0].Trigger)
	assert.False(t, worker.scopes[0].StartedAt.IsZero())

	_, err = runContextWorkerUntilConverged(t.Context(), &scriptedContextWorker{
		results: []embed.RunResult{{Contextual: &embed.ContextConvergence{}}},
	}, vector.GenerationID(1), "contextual-eval:test-stalled")
	require.ErrorContains(t, err, "made no progress")
}

func TestBuildFreshArmIndex_UsesProductionSQLiteBackend(t *testing.T) {
	client := &fixedDimensionEmbedder{dimension: evaluationDimension}
	corpus := Corpus{Scenarios: []Scenario{{ID: "email-001", Family: familyEmail, Query: "synthetic query",
		PositiveID: "email-001-positive", HardNegativeID: "email-001-negative"}},
		Judgments: map[string]Judgment{"email-001": {ScenarioID: "email-001", Grades: map[string]int{"email-001-positive": 3, "email-001-negative": 0}}}}
	sources, mainDB, mainPath, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	documents := buildArmDocuments(ArmNestedContext4, sources[:1])
	index, report, err := buildFreshArmIndex(t.Context(), t.TempDir(), ArmNestedContext4, documents, client, mainDB, mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	assert.Equal(t, context4Model, report.Model)
	assert.Equal(t, "client_batches_excluding_internal_retries", report.RequestAccounting)
	query := make([]float32, evaluationDimension)
	query[0] = 1
	hits, err := index.Search(t.Context(), query, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{sources[0].ID}, hits)
}

func TestScaleSweep_ParsesAndKeepsDistinctResults(t *testing.T) {
	scales, err := parseScales("10000,100000,10000")
	require.NoError(t, err)
	assert.Equal(t, []int{10000, 100000}, scales)
	report := scaleSweepReport{SchemaVersion: 1, ScaleResults: map[string]EvaluationReport{
		"10000": {SchemaVersion: 1}, "100000": {SchemaVersion: 1},
	}}
	payload, err := json.Marshal(report)
	require.NoError(t, err)
	assert.Contains(t, string(payload), "\"10000\"")
	assert.Contains(t, string(payload), "\"100000\"")
}

func TestBlindPool_IsDeterministicOpaqueAndGradeable(t *testing.T) {
	run := evaluationRun{
		Rankings: map[string]map[string][]string{
			ArmOldProduction:  {"chat-001": {"chat-001-positive", "chat-001-negative"}},
			ArmNestedContext4: {"chat-001": {"chat-001-negative", "chat-001-positive"}},
		},
		Queries: map[string]string{"chat-001": "Which synthetic decision was recorded?"},
		Sources: map[string]poolSource{
			"chat-001-positive": {Excerpt: "Conversation context and confirmed action."},
			"chat-001-negative": {Excerpt: "Different context and the same local phrase."},
		},
	}
	first, err := buildBlindPool(run)
	require.NoError(t, err)
	second, err := buildBlindPool(run)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	require.Len(t, first, 1)
	assert.Equal(t, "Which synthetic decision was recorded?", first[0].Query)
	require.Len(t, first[0].Candidates, 2)
	assert.NotEmpty(t, first[0].Candidates[0].Excerpt)
	payload, err := json.Marshal(first)
	require.NoError(t, err)
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{"positive", "negative", "target", "alternate", "grade", ArmOldProduction, ArmNestedContext4} {
		assert.NotContains(t, lower, strings.ToLower(forbidden))
	}
}

func TestReplay_UsesProductionTicksAndReportsPerAppendStress(t *testing.T) {
	events := buildReplayEvents(2)
	require.Len(t, events, 6)
	assert.Equal(t, []replayEventKind{
		replayMutationEvent, replayMutationEvent, replayTickEvent,
		replayMutationEvent, replayMutationEvent, replayTickEvent,
	}, []replayEventKind{events[0].Kind, events[1].Kind, events[2].Kind, events[3].Kind, events[4].Kind, events[5].Kind})
	sevenDayEvents := buildReplayEvents(7)
	bodyLengths := make([]int, 0, 14)
	for _, event := range sevenDayEvents {
		if event.Kind == replayMutationEvent {
			bodyLengths = append(bodyLengths, len([]rune(event.Body)))
		}
	}
	assert.GreaterOrEqual(t, slices.Min(bodyLengths), 80)
	assert.GreaterOrEqual(t, slices.Max(bodyLengths), 500)
	workload := summarizeReplayWorkload(sevenDayEvents)
	assert.Equal(t, 14, workload.Mutations)
	assert.Equal(t, 7, workload.ScheduledTicks)
	assert.Equal(t, slices.Min(bodyLengths), workload.BodyRunes.Min)
	assert.Equal(t, slices.Max(bodyLengths), workload.BodyRunes.Max)
	assert.False(t, workload.CapacityForecast)

	one, err := replayProductionHistory(t.Context(), t.TempDir(), 1)
	require.NoError(t, err)
	legacy := &replaySemanticClient{dimension: evaluationDimension}
	scheduled := &replaySemanticClient{dimension: evaluationDimension}
	stress := &replaySemanticClient{dimension: evaluationDimension}
	seven, err := replayProductionHistoryWithClients(t.Context(), t.TempDir(), sevenDayEvents, replayClients{
		LegacyScheduled: legacy, ContextualScheduled: scheduled, ContextualPerAppendStress: stress,
	})
	require.NoError(t, err)
	assert.Len(t, one.Mutations, 2)
	assert.Len(t, seven.Mutations, 14)
	assert.Len(t, one.Ticks, 1)
	assert.Len(t, seven.Ticks, 7)
	assert.Equal(t, 1, one.ContextualScheduled.EmbeddedDocuments)
	assert.Equal(t, 7, seven.ContextualScheduled.EmbeddedDocuments)
	assert.Equal(t, 14, seven.LegacyScheduled.Publications)
	assert.Zero(t, seven.LegacyScheduled.Replacements)
	assert.Zero(t, seven.ContextualScheduled.Replacements)
	assert.Equal(t, []string{"2030-01-01", "2030-01-02", "2030-01-03", "2030-01-04", "2030-01-05", "2030-01-06", "2030-01-07"},
		scheduled.replayDocumentDays())
	assert.Greater(t, seven.LegacyScheduled.ProviderTokens, one.LegacyScheduled.ProviderTokens)
	assert.Greater(t, seven.ContextualPerAppendStress.ProviderTokens, seven.ContextualScheduled.ProviderTokens)
	assert.Equal(t, seven.LegacyScheduled.ProviderRequests, seven.ContextualScheduled.ProviderRequests)
	assert.Equal(t, int64(7), seven.ContextualScheduled.ProviderRequests)
	for i, mutation := range seven.Mutations {
		assert.Equal(t, i/2+1, mutation.Day)
	}
	for i, tick := range seven.Ticks {
		assert.Equal(t, i+1, tick.Day)
		assert.Equal(t, (i+1)*2, tick.AfterMutations)
	}

	sameDay := buildReplayEvents(1)
	twoTicks := []replayEvent{sameDay[0], {Kind: replayTickEvent, Day: 1}, sameDay[1], {Kind: replayTickEvent, Day: 1}}
	twoTickHistory, err := replayProductionHistoryWithClients(t.Context(), t.TempDir(), twoTicks, replayClients{
		LegacyScheduled:           &replaySemanticClient{dimension: evaluationDimension},
		ContextualScheduled:       &replaySemanticClient{dimension: evaluationDimension},
		ContextualPerAppendStress: &replaySemanticClient{dimension: evaluationDimension},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, twoTickHistory.ContextualScheduled.Publications)
	assert.Equal(t, 2, twoTickHistory.ContextualScheduled.EmbeddedDocuments)
	assert.Equal(t, 1, twoTickHistory.ContextualScheduled.Replacements)

	stressFailure, err := replayProductionHistoryWithClients(t.Context(), t.TempDir(), sameDay, replayClients{
		LegacyScheduled:           &replaySemanticClient{dimension: evaluationDimension},
		ContextualScheduled:       &replaySemanticClient{dimension: evaluationDimension},
		ContextualPerAppendStress: failingReplaySemanticClient{dimension: evaluationDimension},
	})
	require.NoError(t, err)
	assert.True(t, stressFailure.LegacyScheduled.UsageAvailable)
	assert.True(t, stressFailure.ContextualScheduled.UsageAvailable)
	assert.False(t, stressFailure.ContextualPerAppendStress.UsageAvailable)
	assert.NotEmpty(t, stressFailure.ContextualPerAppendStress.Error)
}

func TestReplayCLI_RejectsMissingProviderCredential(t *testing.T) {
	t.Setenv("VOYAGE_API_KEY", "")
	scenarios, judgments := fixturePaths()
	err := runReplay(t.Context(), []string{"--days", "1", "--scenarios", scenarios, "--judgments", judgments,
		"--output", filepath.Join(t.TempDir(), "replay.json")})
	require.ErrorContains(t, err, "VOYAGE_API_KEY")
}

func TestScoreAppendGate_RequiresValidatedAuthenticatedReplay(t *testing.T) {
	report := newReport("run", "commit", "corpus-a", "query-a")
	report.Operational.AppendTokensAvailable = false
	report.evaluateGates()
	assert.False(t, appendTokensGate(t, report).Evaluated)

	replay := replayReport{SchemaVersion: 2, CorpusHash: "corpus-a", TokenAccounting: "observed_provider_usage_total_tokens",
		Gate: GateResult{Name: "append_tokens", Evaluated: true, Passed: true, Value: 4, Limit: "<=5"},
		ProductionHistory: productionReplayHistory{
			LegacyScheduled:           replayPathStats{EmbeddedDocuments: 1, ProviderTokens: 10, ProviderRequests: 1, ProviderSuccessfulResponses: 1, ProviderUsageResponses: 1, UsageAvailable: true},
			ContextualScheduled:       replayPathStats{EmbeddedDocuments: 1, ProviderTokens: 40, ProviderRequests: 1, ProviderSuccessfulResponses: 1, ProviderUsageResponses: 1, UsageAvailable: true},
			ContextualPerAppendStress: replayPathStats{Error: "diagnostic unavailable"},
		}}
	require.NoError(t, applyReplayResult(&report, replay))
	assert.True(t, report.Operational.AppendTokensAvailable)
	assert.InDelta(t, 4.0, report.Operational.AppendTokenAmplification, 0.0001)
	report.evaluateGates()
	assert.True(t, appendTokensGate(t, report).Evaluated)
	replay.ProductionHistory.ContextualScheduled.EmbeddedDocuments = 0
	require.ErrorContains(t, applyReplayResult(&report, replay), "usage is incomplete")
	replay.ProductionHistory.ContextualScheduled.EmbeddedDocuments = 1

	replay.CorpusHash = "corpus-b"
	require.ErrorContains(t, applyReplayResult(&report, replay), "corpus hash")
	replay.CorpusHash = "corpus-a"
	replay.TokenAccounting = "estimated"
	require.ErrorContains(t, applyReplayResult(&report, replay), "token accounting")
}

func TestApplyReplayCLI_RecomposesSavedReportWithoutEmbedding(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	replayPath := filepath.Join(dir, "replay.json")
	outputPath := filepath.Join(dir, "recomposed.json")
	report := newReport("paid-run", "commit-a", "corpus-a", "query-a")
	report.Operational.AppendTokensAvailable = false
	report.evaluateGates()
	replay := replayReport{
		SchemaVersion: 2, CorpusHash: report.CorpusHash, TokenAccounting: "observed_provider_usage_total_tokens",
		Gate: GateResult{Name: "append_tokens", Evaluated: true, Passed: true, Value: 1.5, Limit: "<=5"},
		ProductionHistory: productionReplayHistory{
			LegacyScheduled: replayPathStats{EmbeddedDocuments: 2, ProviderTokens: 100, ProviderRequests: 1,
				ProviderSuccessfulResponses: 1, ProviderUsageResponses: 1, UsageAvailable: true},
			ContextualScheduled: replayPathStats{EmbeddedDocuments: 1, ProviderTokens: 150, ProviderRequests: 1,
				ProviderSuccessfulResponses: 1, ProviderUsageResponses: 1, UsageAvailable: true},
		},
	}
	require.NoError(t, writeJSON(reportPath, report))
	require.NoError(t, writeJSON(replayPath, replay))
	require.NoError(t, run(t.Context(), []string{"apply-replay", "--report", reportPath,
		"--replay-report", replayPath, "--output", outputPath}))

	var recomposed EvaluationReport
	require.NoError(t, readJSONFile(outputPath, &recomposed))
	assert.Equal(t, report.RunID, recomposed.RunID)
	assert.Equal(t, report.Arms, recomposed.Arms)
	assert.True(t, recomposed.Operational.AppendTokensAvailable)
	assert.InDelta(t, 1.5, recomposed.Operational.AppendTokenAmplification, 0.0001)
	assert.True(t, appendTokensGate(t, recomposed).Evaluated)
	assert.True(t, appendTokensGate(t, recomposed).Passed)

	replay.CorpusHash = "wrong-corpus"
	require.NoError(t, writeJSON(replayPath, replay))
	require.ErrorContains(t, run(t.Context(), []string{"apply-replay", "--report", outputPath,
		"--replay-report", replayPath, "--output", outputPath}), "corpus hash")
	var preserved EvaluationReport
	require.NoError(t, readJSONFile(outputPath, &preserved))
	assert.Equal(t, recomposed, preserved)
}

func appendTokensGate(t *testing.T, report EvaluationReport) GateResult {
	t.Helper()
	for _, gate := range report.Gates.Results {
		if gate.Name == "append_tokens" {
			return gate
		}
	}
	require.FailNow(t, "missing append_tokens gate")
	return GateResult{}
}

func TestExactOracleAndVectorDiagnostics(t *testing.T) {
	vectors := map[string][][]float32{
		"a": {{1, 0}},
		"b": {{0.8, 0.2}},
		"c": {{0, 1}},
	}
	cosine := exactTopKChunks(vectors, []float32{1, 0}, similarityCosine, 3)
	l2 := exactTopKChunks(vectors, []float32{1, 0}, similarityL2, 3)
	assert.Equal(t, []string{"a", "b", "c"}, cosine)
	assert.Equal(t, []string{"a", "b", "c"}, l2)
	assert.InDelta(t, 1.0, topKOverlap(cosine, l2, 3), 0.0001)
	assert.InDelta(t, 1, vectorNorm([]float32{0.6, 0.8}), 0.0001)
	assert.True(t, math.IsInf(similarityL2([]float32{1}, []float32{1, 2}), -1))
}

func TestExactANNRecall_UsesExhaustiveL2AndBoundedTopK(t *testing.T) {
	vectors := map[string][][]float32{
		"same-direction-far": {{10, 0}},
		"near-by-l2":         {{0.9, 0.2}},
		"orthogonal":         {{0, 1}},
	}
	query := []float32{1, 0}
	l2 := exactTopKChunks(vectors, query, similarityL2, 2)
	cosine := exactTopKChunks(vectors, query, similarityCosine, 2)
	assert.Equal(t, []string{"near-by-l2", "orthogonal"}, l2)
	assert.Equal(t, []string{"same-direction-far", "near-by-l2"}, cosine)
	metrics := scoreRanking(l2, l2, cosine, Judgment{})
	assert.InDelta(t, 1.0, metrics.ExactANNRecallAt10, 0.0001)
	assert.InDelta(t, 0.5, metrics.L2CosineOverlap10, 0.0001)
	assert.LessOrEqual(t, len(exactTopKChunks(vectors, query, similarityL2, 1)), 1)
}

func TestExactOracleFlagAndScaleGateControlDiagnostics(t *testing.T) {
	disabled := exactDiagnosticPolicy(false, 20000)
	assert.False(t, disabled.Enabled)
	assert.True(t, disabled.ScaleGateEvaluated, "the primary exhaustive L2 ANN gate is mandatory")
	assert.Equal(t, "exhaustive_l2", disabled.ANNOracle)
	enabled := exactDiagnosticPolicy(true, 20000)
	assert.True(t, enabled.Enabled)
	assert.True(t, enabled.ScaleGateEvaluated)
	assert.Equal(t, "exhaustive_l2", enabled.ANNOracle)
	large := exactDiagnosticPolicy(true, 100000)
	assert.True(t, large.ScaleGateEvaluated)
	assert.Equal(t, 100000, large.Scale)
}

func TestHybridRanking_UsesProductionSQLiteFusedSearch(t *testing.T) {
	client := &fixedDimensionEmbedder{dimension: evaluationDimension}
	corpus := Corpus{Scenarios: []Scenario{{ID: "email-001", Family: familyEmail, Query: "rarelexeme",
		PositiveID: "email-001-doc-a", HardNegativeID: "email-001-doc-b"}},
		Judgments: map[string]Judgment{"email-001": {ScenarioID: "email-001", Grades: map[string]int{"email-001-doc-a": 3}}}}
	sources, mainDB, mainPath, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	index, _, err := buildFreshArmIndex(t.Context(), t.TempDir(), ArmNestedContext4,
		buildArmDocuments(ArmNestedContext4, sources), client, mainDB, mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	query := make([]float32, evaluationDimension)
	query[0] = 1
	hits, saturated, err := index.FusedSearch(t.Context(), query, []string{"rarelexeme"}, 10)
	require.NoError(t, err)
	assert.False(t, saturated)
	assert.NotEmpty(t, hits)
	assert.Equal(t, "sqlitevec.FusedSearch", index.hybridImplementation)
}

func TestStructuredCorpus_SeedsProductionFTSAndBM25ChangesVectorOnlyRank(t *testing.T) {
	scenario := Scenario{ID: "chat-001", Family: familyChat, Query: "rarelexeme decision", ContextOnly: true,
		PositiveID: "chat-001-doc-a", HardNegativeID: "chat-001-doc-b"}
	corpus := Corpus{Scenarios: []Scenario{scenario}, Judgments: map[string]Judgment{
		scenario.ID: {ScenarioID: scenario.ID, Grades: map[string]int{scenario.PositiveID: 3, scenario.HardNegativeID: 0}},
	}, Distractors: generateDistractors(3)}
	sources, mainStore, mainPath, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	var messages, indexed int
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	require.NoError(t, mainStore.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&indexed))
	assert.Equal(t, messages, indexed, "every evaluated source and distractor must be in production FTS")

	client := &ftsRankingEmbedder{dimension: evaluationDimension}
	index, _, err := buildFreshArmIndex(t.Context(), t.TempDir(), ArmNestedContext4,
		buildArmDocuments(ArmNestedContext4, sources), client, mainStore, mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	query := make([]float32, evaluationDimension)
	query[0] = 1
	vectorOnly, _, err := index.FusedSearch(t.Context(), query, nil, 10)
	require.NoError(t, err)
	hybrid, _, err := index.FusedSearch(t.Context(), query, []string{"rarelexeme"}, 10)
	require.NoError(t, err)
	assert.NotEqual(t, vectorOnly, hybrid, "BM25 must materially change the vector-only order")
}

func TestTranscriptEvidence_UsesScoreMessageChunksAfterANNMessageHit(t *testing.T) {
	scenario := Scenario{ID: "transcript-001", Family: familyTranscript, Query: "cobalt launch decision", ContextOnly: true,
		PositiveID: "transcript-001-doc-a", HardNegativeID: "transcript-001-doc-b"}
	corpus := Corpus{Scenarios: []Scenario{scenario}, Judgments: map[string]Judgment{
		scenario.ID: {ScenarioID: scenario.ID, Grades: map[string]int{scenario.PositiveID: 3, scenario.HardNegativeID: 0},
			EvidenceIDs: []string{scenario.PositiveID + "-evidence"}},
	}}
	sources, mainStore, mainPath, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	positive := sourceByExternalID(t, sources, scenario.PositiveID)
	answerStart := strings.Index(positive.Body, "Confirmed action")
	require.NotEqual(t, -1, answerStart)
	judgment := corpus.Judgments[scenario.ID]
	judgment.EvidenceRefs = []EvidenceRef{{SourceID: scenario.PositiveID, RawStart: answerStart,
		RawEnd: answerStart + len("Confirmed action 001. Proceed with the recorded step.")}}
	corpus.Judgments[scenario.ID] = judgment
	client := &contextualEvidenceEmbedder{dimension: evaluationDimension}
	index, _, err := buildFreshArmIndex(t.Context(), t.TempDir(), ArmNestedContext4,
		buildArmDocuments(ArmNestedContext4, sources), client, mainStore, mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	query := make([]float32, evaluationDimension)
	query[0] = 1
	ann, err := index.Search(t.Context(), query, 10)
	require.NoError(t, err)
	assert.Contains(t, ann, scenario.PositiveID)
	evidence, err := index.EvidenceRanking(t.Context(), []string{scenario.PositiveID}, query)
	require.NoError(t, err)
	assert.Equal(t, []string{scenario.PositiveID + "-evidence"}, evidence)
	assert.InDelta(t, 1.0, evidenceHitAt(evidence, corpus.Judgments[scenario.ID].EvidenceIDs, 10), 0.0001)
	winners, err := index.EvidenceRefs(t.Context(), []string{scenario.PositiveID}, query)
	require.NoError(t, err)
	assert.InDelta(t, 1.0, evidenceSpanHitAt(winners, corpus.Judgments[scenario.ID].EvidenceRefs, 10), 0.0001)
	output, err := evaluateArmIndex(t.Context(), index, ArmReport{}, armEvalInput{Scenarios: []armScenarioInput{{
		Scenario: scenario, Judgment: corpus.Judgments[scenario.ID], Query: query,
	}}})
	require.NoError(t, err)
	for _, ranker := range []string{"ann", "hybrid"} {
		provenance := output.Provenance[scenario.ID][ranker]
		require.NotEmpty(t, provenance)
		assert.Condition(t, func() bool {
			for _, winner := range provenance {
				if winner.ID == scenario.PositiveID+"-evidence" && winner.SourceID == scenario.PositiveID {
					return winner.MessageID > 0 && winner.DocumentID != ""
				}
			}
			return false
		}, ranker)
	}
}

func TestProductionConfig_IsFrozenInReportAndUsesRealChunkLimit(t *testing.T) {
	config := evaluationVectorConfig(ArmOldProduction)
	assert.Equal(t, 32768, config.Embeddings.MaxInputChars)
	assert.Equal(t, 32, config.Embeddings.BatchSize)
	assert.NotEmpty(t, config.GenerationFingerprint())
	report := armConfigReport(ArmOldProduction, config)
	assert.Equal(t, config.GenerationFingerprint(), report.GenerationFingerprint)
	assert.Equal(t, 32768, report.MaxInputChars)
	assert.Equal(t, "production_worker", report.BuilderBoundary)
}

func TestOldProductionArm_RunsProductionWorkerBatchBoundary(t *testing.T) {
	corpus := Corpus{Scenarios: []Scenario{{ID: "email-001", Family: familyEmail, Query: "synthetic query",
		PositiveID: "email-001-doc-a", HardNegativeID: "email-001-doc-b"}}, Judgments: map[string]Judgment{
		"email-001": {ScenarioID: "email-001", Grades: map[string]int{"email-001-doc-a": 3, "email-001-doc-b": 0}},
	}}
	sources, mainStore, mainPath, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	client := &productionWorkerRecordingEmbedder{dimension: evaluationDimension}
	index, report, err := buildFreshArmIndex(t.Context(), t.TempDir(), ArmOldProduction,
		buildArmDocuments(ArmOldProduction, sources), client, mainStore, mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	assert.NotEmpty(t, client.batchSizes)
	for _, size := range client.batchSizes {
		assert.LessOrEqual(t, size, 32)
	}
	assert.Equal(t, "production_worker", report.Config.BuilderBoundary)
	assert.Equal(t, 32768, report.Config.MaxInputChars)
}

func TestBlindWorkflow_RoundTripsReturnedGradesThroughPrivateMap(t *testing.T) {
	run := evaluationRun{
		Report:   EvaluationReport{CorpusHash: "corpus", QueryHash: "query"},
		Rankings: map[string]map[string][]string{ArmNestedContext4: {"chat-001": {"chat-001-doc-a"}}},
		Queries:  map[string]string{"chat-001": "Which synthetic decision was recorded?"},
		Sources: map[string]poolSource{"chat-001-doc-a": {
			Excerpt: "Confirmed action.", Provenance: chunkProvenance{Family: familyChat, Chunk: "answer"},
		}},
	}
	public, private, err := buildBlindBundle(run)
	require.NoError(t, err)
	require.Len(t, public.Pool, 1)
	assert.Equal(t, "corpus", public.CorpusHash)
	assert.Equal(t, "query", public.QueryHash)
	assert.NotEmpty(t, public.PoolHash)
	assert.Equal(t, public.PoolHash, private.PoolHash)
	handle := public.Pool[0].Candidates[0].Handle
	grades := blindGrades{SchemaVersion: 1, CorpusHash: "corpus", QueryHash: "query", PoolHash: public.PoolHash, Judgments: []blindScenarioJudgment{{
		ScenarioID: "chat-001", Grades: map[string]int{handle: 3}, EvidenceHandles: []string{handle},
	}}}
	unblinded, err := unblindJudgments(grades, private)
	require.NoError(t, err)
	assert.Equal(t, 3, unblinded["chat-001"].Grades["chat-001-doc-a"])
	assert.Equal(t, []string{"chat-001-doc-a"}, unblinded["chat-001"].EvidenceIDs)
	payload, err := json.Marshal(public)
	require.NoError(t, err)
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{"doc-a", "arm", "rank", "relevance", "grade"} {
		assert.NotContains(t, lower, forbidden)
	}
	assert.Contains(t, lower, "provenance")
}

func TestDocumentVectorChunks_KeepSourceSpansDespiteContextHeaders(t *testing.T) {
	document := ArmDocument{Key: "window-1", Chunks: []EvalChunk{
		{MessageID: 11, ChunkIndex: 0, Text: "Alice (09:00): early words", SourceStart: 0, SourceEnd: 11,
			SourceBasis: vector.SourceBasisBody},
		{MessageID: 11, ChunkIndex: 1, Text: "Alice (09:01): later words", SourceStart: 11, SourceEnd: 22,
			SourceBasis: vector.SourceBasisBody},
		{MessageID: 12, ChunkIndex: 0, Text: "Bob (09:02): reply", SourceStart: 0, SourceEnd: 5,
			SourceBasis: vector.SourceBasisBody},
	}}
	vectors := [][]float32{{1}, {2}, {3}}
	chunks := documentVectorChunks(document, vectors)
	require.Len(t, chunks, 3)
	for i, chunk := range chunks {
		assert.Equal(t, document.Chunks[i].SourceStart, chunk.ChunkCharStart)
		assert.Equal(t, document.Chunks[i].SourceEnd, chunk.ChunkCharEnd,
			"context headers in the embedded text must not inflate the stored source span")
	}
	assert.Equal(t, 22, chunks[0].SourceCharLen, "message source length is its maximum span end")
	assert.Equal(t, 22, chunks[1].SourceCharLen)
	assert.Equal(t, 5, chunks[2].SourceCharLen)
}

func TestBuildPoolSources_ExcerptsIsolateSiblingMessages(t *testing.T) {
	sources := []SourceDocument{
		{ID: "chat-001-doc-a", MessageID: 11, DocumentID: "window-1", Family: familyChat,
			StructuredChunks: []string{"Alice: the amber answer"}, StructuredChunkIDs: []string{"chunk-a"}},
		{ID: "chat-001-doc-b", MessageID: 12, DocumentID: "window-1", Family: familyChat,
			StructuredChunks: []string{"Bob: unrelated filler"}, StructuredChunkIDs: []string{"chunk-b"}},
	}
	pool := buildPoolSources(sources)
	require.Len(t, pool, 2)
	assert.Contains(t, pool["chat-001-doc-a"].Excerpt, "amber answer")
	assert.NotContains(t, pool["chat-001-doc-a"].Excerpt, "unrelated filler",
		"a candidate's excerpt must not leak sibling messages' text and misattribute blind grades")
	assert.NotContains(t, pool["chat-001-doc-b"].Excerpt, "amber answer")
	assert.Equal(t, "chunk-a", pool["chat-001-doc-a"].ChunkID)
}

func TestBlindWorkflow_BindsHashesProvenanceAndANNHybridUnion(t *testing.T) {
	run := evaluationRun{
		Report:         EvaluationReport{CorpusHash: "corpus-a", QueryHash: "query-a"},
		Rankings:       map[string]map[string][]string{ArmNestedContext4: {"chat-001": {"chat-001-doc-a"}}},
		HybridRankings: map[string]map[string][]string{ArmNestedContext4: {"chat-001": {"chat-001-doc-b"}}},
		Queries:        map[string]string{"chat-001": "Which synthetic decision was recorded?"},
		WinningProvenance: map[string]map[string]map[string][]chunkEvidence{
			ArmNestedContext4: {"chat-001": {
				"ann":    {{ID: "actual-answer-a", SourceID: "chat-001-doc-a", MessageID: 11, ChunkIndex: 2, DocumentID: "window-a", Start: 40, End: 90}},
				"hybrid": {{ID: "actual-answer-b", SourceID: "chat-001-doc-b", MessageID: 12, ChunkIndex: 3, DocumentID: "window-b", Start: 50, End: 100}},
			}},
		},
		Sources: map[string]poolSource{
			"chat-001-doc-a": {Excerpt: "Context window A. Confirmed action.", MessageID: 11, ChunkID: "answer-a", DocumentID: "window-a"},
			"chat-001-doc-b": {Excerpt: "Context window B. Confirmed action.", MessageID: 12, ChunkID: "answer-b", DocumentID: "window-b"},
		},
	}
	public, private, err := buildBlindBundle(run)
	require.NoError(t, err)
	require.Len(t, public.Pool, 1)
	assert.Len(t, public.Pool[0].Candidates, 2, "blind pool must union ANN and hybrid top-20")
	assert.Equal(t, "corpus-a", private.CorpusHash)
	assert.Equal(t, "query-a", private.QueryHash)
	assert.Equal(t, public.PoolHash, private.PoolHash)
	require.NoError(t, validateBlindBundle(run, private))
	tampered := private
	tampered.Handles = maps.Clone(private.Handles)
	for handle, candidate := range tampered.Handles {
		candidate.SourceID = "tampered"
		tampered.Handles[handle] = candidate
		break
	}
	require.ErrorContains(t, validateBlindBundle(run, tampered), "manifest")
	for _, mapped := range private.Handles {
		assert.Positive(t, mapped.MessageID)
		assert.NotEmpty(t, mapped.ChunkID)
		assert.NotEmpty(t, mapped.DocumentID)
		require.Len(t, mapped.Wins, 1)
		assert.Contains(t, mapped.Wins[0].ChunkID, "actual-answer")
	}

	grades := blindGrades{SchemaVersion: 1, CorpusHash: "corpus-a", QueryHash: "query-a", PoolHash: public.PoolHash,
		Judgments: []blindScenarioJudgment{{ScenarioID: "chat-001", Grades: map[string]int{}}}}
	for handle := range private.Handles {
		grades.Judgments[0].Grades[handle] = 3
	}
	_, err = unblindJudgments(grades, private)
	require.NoError(t, err)
	badHash := grades
	badHash.CorpusHash = "corpus-b"
	_, err = unblindJudgments(badHash, private)
	require.ErrorContains(t, err, "corpus hash")
	badGrade := grades
	badGrade.Judgments = append([]blindScenarioJudgment(nil), grades.Judgments...)
	badGrade.Judgments[0].Grades = map[string]int{}
	for handle := range private.Handles {
		badGrade.Judgments[0].Grades[handle] = 4
		break
	}
	_, err = unblindJudgments(badGrade, private)
	require.ErrorContains(t, err, "grade")
	duplicate := grades
	duplicate.Judgments = append(duplicate.Judgments, duplicate.Judgments[0])
	_, err = unblindJudgments(duplicate, private)
	require.ErrorContains(t, err, "duplicate")
}

func TestBlindWorkflow_ScoresBothANNAndHybridRankingsFromFiles(t *testing.T) {
	run := evaluationRun{
		Report:         EvaluationReport{CorpusHash: "corpus-a", QueryHash: "query-a"},
		Rankings:       map[string]map[string][]string{ArmNestedContext4: {"chat-001": {"chat-001-doc-a"}}},
		HybridRankings: map[string]map[string][]string{ArmNestedContext4: {"chat-001": {"chat-001-doc-b"}}},
		Queries:        map[string]string{"chat-001": "Which synthetic decision was recorded?"},
		Sources: map[string]poolSource{
			"chat-001-doc-a": {Excerpt: "Context window A. Confirmed action.", MessageID: 11, ChunkID: "answer-a", DocumentID: "window-a"},
			"chat-001-doc-b": {Excerpt: "Context window B. Confirmed action.", MessageID: 12, ChunkID: "answer-b", DocumentID: "window-b"},
		},
	}
	_, private, err := buildBlindBundle(run)
	require.NoError(t, err)
	grades := blindGrades{SchemaVersion: 1, CorpusHash: "corpus-a", QueryHash: "query-a", PoolHash: private.PoolHash,
		Judgments: []blindScenarioJudgment{{ScenarioID: "chat-001", Grades: map[string]int{}}}}
	for handle, candidate := range private.Handles {
		grade := 1
		if candidate.SourceID == "chat-001-doc-b" {
			grade = 3
		}
		grades.Judgments[0].Grades[handle] = grade
	}
	dir := t.TempDir()
	write := func(name string, value any) string {
		payload, err := json.Marshal(value)
		require.NoError(t, err)
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, payload, 0o600))
		return path
	}
	report, err := scoreBlindFiles(write("manifest.json", run), write("private.json", private), write("grades.json", grades))
	require.NoError(t, err)
	ann := report.ByArm[ArmNestedContext4]["chat-001"]
	hybrid, ok := report.HybridByArm[ArmNestedContext4]["chat-001"]
	require.True(t, ok, "returned blind judgments must also score hybrid rankings")
	assert.Positive(t, ann.NDCGAt10)
	assert.Greater(t, hybrid.NDCGAt10, ann.NDCGAt10,
		"the hybrid ranking placed the grade-3 candidate first and must outscore the ANN ranking")
}

func TestOldContext4Chunks_UseProductionOverlapPolicy(t *testing.T) {
	scenario := Scenario{ID: "transcript-001", Family: familyTranscript, Query: "cobalt launch decision", ContextOnly: true,
		PositiveID: "transcript-001-doc-a", HardNegativeID: "transcript-001-doc-b"}
	documents, cleanup, err := assembleScenarioDocuments(t.Context(), t.TempDir(), scenario)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	positive := sourceByExternalID(t, documents, scenario.PositiveID)
	require.GreaterOrEqual(t, len(positive.OldChunks), 2)
	assert.Equal(t, embed.EmbeddingChunkPolicy(evaluationChunkRunes).OverlapRunes,
		suffixPrefixRuneOverlap(positive.OldChunks[0], positive.OldChunks[1]))
	assert.Equal(t, "production_preprocess_chunk_adapter",
		armConfigReport(ArmOldContext4Singleton, evaluationVectorConfig(ArmOldContext4Singleton)).BuilderBoundary)
}

func TestMaterializedCorpusHash_CoversDocumentsAndPolicy(t *testing.T) {
	corpus := Corpus{Scenarios: []Scenario{{ID: "email-001", Family: familyEmail, Query: "synthetic query",
		PositiveID: "email-001-doc-a", HardNegativeID: "email-001-doc-b"}}, Judgments: map[string]Judgment{
		"email-001": {ScenarioID: "email-001", Grades: map[string]int{"email-001-doc-a": 3, "email-001-doc-b": 0}},
	}}
	sources, _, _, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	base := materializedCorpusHash(corpus, sources, "policy-a")
	changed := append([]SourceDocument(nil), sources...)
	changed[0].StructuredChunks = append([]string(nil), changed[0].StructuredChunks...)
	changed[0].StructuredChunks[0] += " changed"
	assert.NotEqual(t, base, materializedCorpusHash(corpus, changed, "policy-a"))
	assert.NotEqual(t, base, materializedCorpusHash(corpus, sources, "policy-b"))
}

func TestIndexSize_CheckpointsAndIncludesWAL(t *testing.T) {
	client := &fixedDimensionEmbedder{dimension: evaluationDimension}
	corpus := Corpus{Scenarios: []Scenario{{ID: "email-001", Family: familyEmail, Query: "synthetic query",
		PositiveID: "email-001-doc-a", HardNegativeID: "email-001-doc-b"}}, Judgments: map[string]Judgment{
		"email-001": {ScenarioID: "email-001", Grades: map[string]int{"email-001-doc-a": 3, "email-001-doc-b": 0}},
	}}
	sources, mainStore, mainPath, cleanup, err := assembleStructuredCorpus(t.Context(), filepath.Join(t.TempDir(), "source"), corpus)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	index, report, err := buildFreshArmIndex(t.Context(), t.TempDir(), ArmNestedContext4,
		buildArmDocuments(ArmNestedContext4, sources), client, mainStore, mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	want, err := indexFilesBytes(index.path)
	require.NoError(t, err)
	assert.Equal(t, want, report.Build.IndexBytes)
}

func TestWriteJSON_DoesNotClobberDestinationOnEncodeFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	require.NoError(t, os.WriteFile(path, []byte("stable"), 0o600))
	err := writeJSON(path, make(chan int))
	require.Error(t, err)
	payload, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "stable", string(payload))
	matches, globErr := filepath.Glob(path + ".tmp-*")
	require.NoError(t, globErr)
	assert.Empty(t, matches)
}

func suffixPrefixRuneOverlap(left, right string) int {
	leftRunes, rightRunes := []rune(left), []rune(right)
	for size := min(len(leftRunes), len(rightRunes)); size > 0; size-- {
		if string(leftRunes[len(leftRunes)-size:]) == string(rightRunes[:size]) {
			return size
		}
	}
	return 0
}

func TestScaleSweep_UsesFreshRunDirectoryForEveryInvocation(t *testing.T) {
	first := scaleRunDirectories("/tmp/eval", "run-a", []int{10000, 100000})
	second := scaleRunDirectories("/tmp/eval", "run-b", []int{10000, 100000})
	assert.NotEqual(t, first[10000], second[10000])
	assert.NotEqual(t, first[10000], first[100000])
	assert.Contains(t, first[10000], "run-a")
}

func TestReplay_ChangesProductionPublicationHistoryWithDays(t *testing.T) {
	one, err := replayProductionHistory(t.Context(), t.TempDir(), 1)
	require.NoError(t, err)
	seven, err := replayProductionHistory(t.Context(), t.TempDir(), 7)
	require.NoError(t, err)
	assert.NotEqual(t, one.Mutations, seven.Mutations)
	assert.Greater(t, seven.ContextualPerAppendStress.Publications, seven.ContextualScheduled.Publications)
	assert.Positive(t, seven.ContextualPerAppendStress.Replacements)
	assert.Equal(t, "shared_mutations_and_production_ticks_worker_context_workers_publication_ledger", seven.Path)
	assert.Equal(t, seven.LegacyScheduled.ProviderRequests, seven.ContextualScheduled.ProviderRequests)
	assert.Greater(t, seven.ContextualPerAppendStress.ProviderRequests, seven.ContextualScheduled.ProviderRequests)
	assert.Len(t, seven.Ticks, 7)
}

func TestHTTPAttemptObserver_CountsRetriesStatusesErrorsAndUsage(t *testing.T) {
	attempt := 0
	handlerErr := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"data":[],"usage":{"total_tokens":17}}`))
		handlerErr <- err
	}))
	t.Cleanup(upstream.Close)
	proxy, observer, err := newCountingEmbeddingProxy(upstream.URL)
	require.NoError(t, err)
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	for range 2 {
		response, err := http.Post(server.URL+"/embeddings", "application/json", strings.NewReader(`{"input":["redacted"]}`))
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	require.NoError(t, <-handlerErr)
	snapshot := observer.Snapshot()
	assert.Equal(t, int64(2), snapshot.Attempts)
	assert.Equal(t, map[int]int64{200: 1, 429: 1}, snapshot.Statuses)
	assert.Equal(t, int64(17), snapshot.ResponseTokens)
	assert.Equal(t, int64(1), snapshot.ResponseDocuments)
	assert.Empty(t, snapshot.ErrorMessages)
	assert.False(t, replayUsageAvailable(snapshot), "a retried scheduled measurement must fail closed")
	assert.True(t, replayUsageAvailable(HTTPAttemptSnapshot{Attempts: 1, SuccessfulResponses: 1,
		ResponseTokens: 17, ResponseDocuments: 1, UsageResponses: 1, Statuses: map[int]int64{http.StatusOK: 1}}))
	delta := httpAttemptDelta(HTTPAttemptSnapshot{Statuses: map[int]int64{}}, snapshot)
	assert.Positive(t, delta.LatencyMillis.P95, "phase deltas must preserve physical-attempt latency")
	payload, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "redacted")
}

func TestHTTPAttemptAggregation_MergesQueryAndDocumentSamples(t *testing.T) {
	report := observedArmReport(ArmNestedContext4)
	query := HTTPAttemptSnapshot{Attempts: 2, SuccessfulResponses: 2, UsageResponses: 2, ResponseTokens: 4,
		Statuses: map[int]int64{200: 2}, LatencyMillis: summarizeLatencies([]time.Duration{time.Millisecond, 3 * time.Millisecond}),
		latencySamples: []time.Duration{time.Millisecond, 3 * time.Millisecond}}
	documents := HTTPAttemptSnapshot{Attempts: 2, SuccessfulResponses: 2, UsageResponses: 2, ResponseTokens: 8,
		Statuses: map[int]int64{200: 2}, LatencyMillis: summarizeLatencies([]time.Duration{5 * time.Millisecond, 7 * time.Millisecond}),
		latencySamples: []time.Duration{5 * time.Millisecond, 7 * time.Millisecond}}
	applyHTTPObservation(&report, query, "query")
	applyHTTPObservation(&report, documents, "document")
	assert.Equal(t, summarizeLatencies([]time.Duration{time.Millisecond, 3 * time.Millisecond, 5 * time.Millisecond, 7 * time.Millisecond}), report.LatencyMillis)
	assert.Equal(t, query.LatencyMillis, report.QueryLatencyMillis)
	assert.Equal(t, documents.LatencyMillis, report.DocumentLatencyMillis)
}

func TestSummarizeLatencies_PreservesSubMicrosecondAttempt(t *testing.T) {
	summary := summarizeLatencies([]time.Duration{time.Nanosecond})

	assert.Positive(t, summary.P50)
	assert.Positive(t, summary.P95)
	assert.Positive(t, summary.P99)
	assert.Equal(t, time.Nanosecond, retainMeasuredLatency(0))
	assert.Equal(t, time.Microsecond, retainMeasuredLatency(time.Microsecond))
}

func TestHTTPAttemptObserver_SeesProductionClientPhysicalRetries(t *testing.T) {
	tests := []struct {
		name string
		call func(string) error
	}{
		{name: "openai", call: func(endpoint string) error {
			client := embed.NewClient(embed.Config{Endpoint: endpoint, APIKey: "synthetic-secret", Model: oldProductionModel,
				Dimension: 2, MaxRetries: 2, Timeout: time.Second})
			_, err := client.EmbedQuery(t.Context(), "synthetic query")
			return err
		}},
		{name: "voyage", call: func(endpoint string) error {
			client := embed.NewVoyageClient(embed.VoyageConfig{Endpoint: endpoint, APIKey: "synthetic-secret", Model: context4Model,
				Dimension: 2, MaxRetries: 2, Timeout: time.Second})
			_, err := client.EmbedQuery(t.Context(), "synthetic query")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempt := 0
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				assert.Equal(t, strings.TrimPrefix(upstream.URL, "http://"), request.Host)
				assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
				attempt++
				if attempt == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(request.URL.Path, "/contextualizedembeddings") {
					_, _ = w.Write([]byte(`{"data":[{"index":0,"data":[{"index":0,"embedding":[1,0]}]}],"usage":{"total_tokens":11}}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}],"usage":{"total_tokens":7}}`))
			}))
			t.Cleanup(upstream.Close)
			proxy, observer, err := newCountingEmbeddingProxy(upstream.URL)
			require.NoError(t, err)
			server := httptest.NewServer(proxy)
			t.Cleanup(server.Close)
			require.NoError(t, test.call(server.URL))
			snapshot := observer.Snapshot()
			assert.Equal(t, int64(2), snapshot.Attempts)
			assert.Equal(t, int64(1), snapshot.Statuses[http.StatusTooManyRequests])
			assert.Equal(t, int64(1), snapshot.Statuses[http.StatusOK])
			assert.Positive(t, snapshot.ResponseTokens)
			payload, err := json.Marshal(snapshot)
			require.NoError(t, err)
			assert.NotContains(t, string(payload), "synthetic query")
			assert.NotContains(t, string(payload), "synthetic-secret")
		})
	}
}

func TestOperationalGate_IsUnevaluatedWithoutIsolatedMaxRSS(t *testing.T) {
	report := newReport("run", "commit", "corpus", "query")
	report.Operational.PeakRSSRatio = nil
	report.evaluateGates()
	for _, gate := range report.Gates.Results {
		if gate.Name == "peak_rss_ratio" {
			assert.False(t, gate.Evaluated)
			assert.False(t, gate.Passed)
			return
		}
	}
	require.Fail(t, "peak RSS gate is missing")
}

func TestOperationalGate_PositiveCandidateWithZeroBaselineFailsClosed(t *testing.T) {
	report := newReport("run-synthetic", "commit-synthetic", "corpus-hash", "query-hash")
	report.Operational.RebuildTimeRatio = safeRatio(1, 0)
	report.evaluateGates()

	assert.InDelta(t, math.MaxFloat64, report.Operational.RebuildTimeRatio, 0)
	for _, gate := range report.Gates.Results {
		if gate.Name != "rebuild_time" {
			continue
		}
		assert.True(t, gate.Evaluated)
		assert.False(t, gate.Passed, "a positive candidate over an unmeasurable baseline must not pass")
		return
	}
	require.Fail(t, "rebuild_time gate is missing")
}

func TestMeasuredChild_ReportsIsolatedMaxRSSAndPartialFailure(t *testing.T) {
	const helperModeEnv = "MSGVAULT_EVAL_MEASURED_CHILD_TEST_MODE"
	switch os.Getenv(helperModeEnv) {
	case "success":
		fmt.Print("child-ok")
		os.Exit(0)
	case "failure":
		fmt.Print("partial-report")
		os.Exit(7)
	}

	helperCommand := func(mode string) *exec.Cmd {
		// #nosec G702 -- the command is the current Go test executable; mode is passed only through the environment.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMeasuredChild_ReportsIsolatedMaxRSSAndPartialFailure$")
		cmd.Env = append(os.Environ(), helperModeEnv+"="+mode)
		return cmd
	}
	success := runMeasuredChildCommand(helperCommand("success"))
	assert.Equal(t, "child-ok", string(success.Output))
	assert.Empty(t, success.ExitError)
	switch runtime.GOOS {
	case "linux":
		require.NotNil(t, success.MaxRSSBytes)
		assert.Positive(t, *success.MaxRSSBytes)
		assert.Equal(t, "linux_wait4_child_maxrss", success.MemoryMeasurement)
	case "darwin":
		require.NotNil(t, success.MaxRSSBytes)
		assert.Positive(t, *success.MaxRSSBytes)
		assert.Equal(t, "darwin_child_maxrss", success.MemoryMeasurement)
	default:
		assert.Nil(t, success.MaxRSSBytes)
		assert.Equal(t, "unavailable", success.MemoryMeasurement)
	}

	failure := runMeasuredChildCommand(helperCommand("failure"))
	assert.Equal(t, "partial-report", string(failure.Output))
	assert.NotEmpty(t, failure.ExitError)
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		require.NotNil(t, failure.MaxRSSBytes)
	} else {
		assert.Nil(t, failure.MaxRSSBytes)
	}
}

func TestBuiltExecutable_UsesProductionChildrenFTSExactUsageAndReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go-build integration test in -short mode")
	}
	dir := t.TempDir()
	binaryName := "contextual-retrieval-eval"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(dir, binaryName)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5 sqlite_vec", "-o", binary, ".")
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, string(buildOutput))

	provider := newDeterministicEmbeddingProvider()
	t.Cleanup(provider.Close)
	scenarios := filepath.Join(dir, "scenarios.jsonl")
	judgments := filepath.Join(dir, "judgments.jsonl")
	require.NoError(t, os.WriteFile(scenarios, []byte(`{"id":"email-001","family":"email","query":"synthetic executable query","context_only":false,"positive_id":"email-001-doc-a","hard_negative_id":"email-001-doc-b"}`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(judgments, []byte(`{"scenario_id":"email-001","grades":{"email-001-doc-a":3,"email-001-doc-b":0}}`+"\n"), 0o600))

	missingKeyReplay := exec.CommandContext(t.Context(), binary, "replay", "--days", "1", "--scenarios", scenarios,
		"--judgments", judgments, "--distractors", "2", "--endpoint", provider.URL,
		"--output", filepath.Join(dir, "missing-key-replay.json"))
	missingKeyReplay.Env = append(os.Environ(), "VOYAGE_API_KEY=")
	missingKeyOutput, err := missingKeyReplay.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(missingKeyOutput), "VOYAGE_API_KEY")

	work := filepath.Join(dir, "work")
	replayPath := filepath.Join(dir, "replay.json")
	replay := exec.CommandContext(t.Context(), binary, "replay", "--days", "2", "--scenarios", scenarios,
		"--judgments", judgments, "--distractors", "2", "--endpoint", provider.URL, "--output", replayPath)
	replay.Env = append(os.Environ(), "VOYAGE_API_KEY=integration-secret")
	replayOutput, err := replay.CombinedOutput()
	require.NoError(t, err, string(replayOutput))
	var replayResult replayReport
	require.NoError(t, readJSONFile(replayPath, &replayResult))

	reportPath := filepath.Join(dir, "report.json")
	score := exec.CommandContext(t.Context(), binary, "score", "--scenarios", scenarios, "--judgments", judgments,
		"--distractors", "2", "--bootstrap", "10", "--work-dir", work, "--endpoint", provider.URL,
		"--replay-report", replayPath, "--output", reportPath)
	score.Env = append(os.Environ(), "VOYAGE_API_KEY=integration-secret")
	scoreOutput, err := score.CombinedOutput()
	require.NoError(t, err, string(scoreOutput))
	var report EvaluationReport
	require.NoError(t, readJSONFile(reportPath, &report))
	assert.True(t, report.Operational.ExactEvaluated)
	assert.True(t, report.Operational.AppendTokensAvailable)
	assert.Equal(t, report.SourceRows, report.FTSRows)
	assert.Positive(t, report.FTSRows)
	for _, arm := range []string{ArmOldProduction, ArmOldContext4Singleton, ArmStructuredSingleton, ArmNestedContext4} {
		armReport := report.Arms[arm]
		assert.True(t, armReport.UsageAvailable, arm)
		assert.Equal(t, armReport.SuccessfulResponses, armReport.UsageResponses, arm)
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			require.NotNil(t, armReport.Build.PeakRSSBytes, arm)
			assert.Positive(t, *armReport.Build.PeakRSSBytes, arm)
		} else {
			assert.Nil(t, armReport.Build.PeakRSSBytes, arm)
			assert.Equal(t, "unavailable", armReport.Build.MemoryMeasurement, arm)
		}
		_, statErr := os.Stat(filepath.Join(work, arm+".db"))
		require.NoError(t, statErr, arm)
	}

	noReplayPath := filepath.Join(dir, "report-no-replay.json")
	noReplay := exec.CommandContext(t.Context(), binary, "score", "--scenarios", scenarios, "--judgments", judgments,
		"--distractors", "2", "--bootstrap", "10", "--work-dir", filepath.Join(dir, "work-no-replay"),
		"--endpoint", provider.URL, "--output", noReplayPath)
	noReplay.Env = append(os.Environ(), "VOYAGE_API_KEY=integration-secret")
	noReplayOutput, err := noReplay.CombinedOutput()
	require.NoError(t, err, string(noReplayOutput))
	var reportWithoutReplay EvaluationReport
	require.NoError(t, readJSONFile(noReplayPath, &reportWithoutReplay))
	assert.False(t, reportWithoutReplay.Operational.AppendTokensAvailable)
	assert.False(t, appendTokensGate(t, reportWithoutReplay).Evaluated)

	assert.Equal(t, "shared_mutations_and_production_ticks_worker_context_workers_publication_ledger", replayResult.ProductionHistory.Path)
	assert.Len(t, replayResult.ProductionHistory.Mutations, 4)
	assert.Len(t, replayResult.ProductionHistory.Ticks, 2)
	assert.Equal(t, []int{1, 1, 2, 2}, []int{
		replayResult.ProductionHistory.Mutations[0].Day, replayResult.ProductionHistory.Mutations[1].Day,
		replayResult.ProductionHistory.Mutations[2].Day, replayResult.ProductionHistory.Mutations[3].Day})
	for _, mode := range []replayPathStats{replayResult.ProductionHistory.LegacyScheduled,
		replayResult.ProductionHistory.ContextualScheduled, replayResult.ProductionHistory.ContextualPerAppendStress} {
		assert.True(t, mode.UsageAvailable)
		assert.Positive(t, mode.ProviderTokens)
		assert.Positive(t, mode.ProviderRequests)
	}
	combined, err := json.Marshal([]any{report, replayResult})
	require.NoError(t, err)
	assert.NotContains(t, string(combined), "integration-secret")
	assert.NotContains(t, string(combined), "synthetic executable query")
}

func newDeterministicEmbeddingProvider() *httptest.Server {
	vector := make([]float32, evaluationDimension)
	vector[0] = 1
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/contextualizedembeddings") {
			var payload struct {
				Inputs [][]string `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid contextual request", http.StatusBadRequest)
				return
			}
			data := make([]map[string]any, len(payload.Inputs))
			tokens := 0
			for i, document := range payload.Inputs {
				chunks := make([]map[string]any, len(document))
				for j := range document {
					chunks[j] = map[string]any{"index": j, "embedding": vector}
					tokens += 3
				}
				data[i] = map[string]any{"index": i, "data": chunks}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]int{"total_tokens": tokens}})
			return
		}
		var payload struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid embedding request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(payload.Input))
		for i := range payload.Input {
			data[i] = map[string]any{"index": i, "embedding": vector}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data,
			"usage": map[string]int{"total_tokens": max(1, len(payload.Input)*2)}})
	}))
}

func TestExecuteEvaluation_EmitsPartialReportOnProviderFailure(t *testing.T) {
	dir := t.TempDir()
	scenarios := filepath.Join(dir, "scenarios.jsonl")
	judgments := filepath.Join(dir, "judgments.jsonl")
	require.NoError(t, os.WriteFile(scenarios, []byte(`{"id":"email-001","family":"email","query":"synthetic query","context_only":false,"positive_id":"email-001-doc-a","hard_negative_id":"email-001-doc-b"}`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(judgments, []byte(`{"scenario_id":"email-001","grades":{"email-001-doc-a":3,"email-001-doc-b":0}}`+"\n"), 0o600))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"synthetic auth failure"}`))
	}))
	t.Cleanup(upstream.Close)
	run, err := executeEvaluation(t.Context(), runConfig{ScenarioPath: scenarios, JudgmentPath: judgments,
		Endpoint: upstream.URL, WorkDir: filepath.Join(dir, "work"), Bootstrap: 10})
	require.Error(t, err)
	assert.False(t, run.Report.Complete)
	assert.NotEmpty(t, run.Report.RunErrors)
	assert.False(t, run.Report.Gates.Passed)
	observed := run.Report.Arms[ArmOldProduction]
	assert.Positive(t, observed.QueryRequests, "query failure must retain observed attempts")
	assert.Positive(t, observed.Errors, "query failure must retain observed HTTP errors")
	assert.Positive(t, observed.HTTPStatuses[http.StatusUnauthorized])
	assert.Positive(t, observed.LatencyMillis.P95, "query failure must retain phase latency")
	for _, gate := range run.Report.Gates.Results {
		if gate.Name == "provider_tokens" {
			assert.False(t, gate.Evaluated, "missing provider usage must fail closed")
		}
	}
	payload, marshalErr := json.Marshal(run.Report)
	require.NoError(t, marshalErr)
	assert.NotContains(t, string(payload), "synthetic query")
	assert.NotContains(t, string(payload), "synthetic auth failure")
}

func TestPairedBootstrap_ResamplesWholeScenariosDeterministically(t *testing.T) {
	pairs := []ScenarioPair{
		{ScenarioID: "chat-001", Family: familyChat, Baseline: 0.1, Candidate: 0.4},
		{ScenarioID: "chat-002", Family: familyChat, Baseline: 0.2, Candidate: 0.3},
		{ScenarioID: "transcript-001", Family: familyTranscript, Baseline: 0.5, Candidate: 0.9},
	}
	first := pairedBootstrap(pairs, 10000, 550)
	second := pairedBootstrap(pairs, 10000, 550)
	assert.Equal(t, first, second)
	assert.InDelta(t, 0.266666, first.Point, 0.0001)
	assert.Greater(t, first.Lower, 0.0)
	assert.GreaterOrEqual(t, first.Upper, first.Point)
}

func TestReport_ContainsQualityOperationalAndAuditFields(t *testing.T) {
	report := newReport("run-synthetic", "commit-synthetic", "corpus-hash", "query-hash")
	report.Quality.ContextualMacro = Interval{Point: 0.04, Lower: 0.01, Upper: 0.07}
	report.Quality.ContextualByFamily = map[string]Interval{
		familyChat:       {Point: 0.05, Lower: 0.01, Upper: 0.09},
		familyTranscript: {Point: 0.04, Lower: 0.01, Upper: 0.08},
	}
	report.Quality.EndToEnd = Interval{Point: 0.06, Lower: 0.02, Upper: 0.1}
	report.Quality.EmailOrIndependent = Interval{Point: 0, Lower: -0.01, Upper: 0.01}
	report.Quality.HybridContextual = Interval{Point: 0.03, Lower: 0.01, Upper: 0.05}
	report.Quality.HybridOverall = Interval{Point: 0.01, Lower: 0, Upper: 0.02}
	report.Operational.ANNRecallAt10 = 0.995
	report.Operational.ExactEvaluated = true
	report.Operational.CachedANNP95Ratio = 1.05
	report.Operational.EndToEndP95Ratio = 1.2
	report.Operational.EndToEndP95Millis = 900
	report.Operational.RebuildTimeRatio = 1.4
	peakRSSRatio := 1.2
	report.Operational.PeakRSSRatio = &peakRSSRatio
	report.Operational.IndexSizeRatio = 1.2
	report.Operational.ProviderTokenRatio = 1.2
	report.Operational.ProviderTokensAvailable = true
	report.Operational.RequestErrorRate = 0.0005
	report.Operational.AppendTokensAvailable = true
	report.Operational.AppendTokenAmplification = 4
	report.evaluateGates()
	assert.True(t, report.Gates.Passed)
	assert.NotEmpty(t, report.Gates.Results)
	require.NotNil(t, report.Arms)
	peakRSSBytes := int64(5)
	report.Arms[ArmNestedContext4] = ArmReport{Model: "voyage-context-4", Requests: 2, InputTokens: 10,
		Errors: 0, LatencyMillis: LatencySummary{P50: 1, P95: 2, P99: 3},
		Build: ResourceSummary{WallMillis: 4, PeakRSSBytes: &peakRSSBytes, MemoryMeasurement: "test", IndexBytes: 6}}
	payload, err := json.Marshal(report)
	require.NoError(t, err)
	for _, field := range []string{"\"gates\"", "\"requests\"", "\"input_tokens\"", "\"errors\"", "\"latency_ms\"", "\"build\""} {
		assert.Contains(t, string(payload), field)
	}
}

func TestCommandBoundary_IsStable(t *testing.T) {
	assert.Equal(t, []string{"generate", "embed", "pool", "score", "replay", "apply-replay"}, commandNames())
}

type countingEmbedder struct {
	queryCalls  int
	queryVector []float32
}

type batchRecordingEmbedder struct {
	batchSizes []int
	next       int
	dimension  int
}

type scriptedContextWorker struct {
	results []embed.RunResult
	scopes  []operations.PassScope
	next    int
}

func (w *scriptedContextWorker) RunOnce(
	_ context.Context, _ vector.GenerationID, scope operations.PassScope,
) (embed.RunResult, error) {
	w.scopes = append(w.scopes, scope)
	if w.next >= len(w.results) {
		return embed.RunResult{Contextual: &embed.ContextConvergence{}}, nil
	}
	result := w.results[w.next]
	w.next++
	return result, nil
}

type fixedDimensionEmbedder struct {
	dimension int
	err       error
}

type failingReplaySemanticClient struct {
	dimension int
}

func (c failingReplaySemanticClient) EmbedQuery(context.Context, string) ([]float32, error) {
	return make([]float32, c.dimension), nil
}

func (failingReplaySemanticClient) EmbedDocuments(context.Context, []embed.DocumentInput) ([][][]float32, error) {
	return nil, errors.New("synthetic stress diagnostic failure")
}

type productionWorkerRecordingEmbedder struct {
	dimension  int
	batchSizes []int
	err        error
}

func (c *productionWorkerRecordingEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	c.batchSizes = append(c.batchSizes, len(inputs))
	result := make([][]float32, len(inputs))
	for i := range inputs {
		result[i] = make([]float32, c.dimension)
		result[i][0] = 1
	}
	return result, c.err
}

func (c *productionWorkerRecordingEmbedder) EmbedDocuments(context.Context, []embed.DocumentInput) ([][][]float32, error) {
	return nil, errors.New("O-prod must not use contextual document embedding")
}

type contextualEvidenceEmbedder struct {
	dimension int
	err       error
}

type ftsRankingEmbedder struct {
	dimension int
	err       error
}

func (c *ftsRankingEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	result := make([]float32, c.dimension)
	result[0] = 1
	return result, c.err
}

func (c *ftsRankingEmbedder) EmbedDocuments(_ context.Context, documents []embed.DocumentInput) ([][][]float32, error) {
	result := make([][][]float32, len(documents))
	for i, document := range documents {
		result[i] = make([][]float32, len(document.Chunks))
		for j, chunk := range document.Chunks {
			result[i][j] = make([]float32, c.dimension)
			if strings.Contains(chunk, "matched unrelated synthetic comparison") {
				result[i][j][0] = 1
			} else {
				result[i][j][1] = 1
			}
		}
	}
	return result, c.err
}

func (c *contextualEvidenceEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	result := make([]float32, c.dimension)
	result[0] = 1
	return result, c.err
}

func (c *contextualEvidenceEmbedder) EmbedDocuments(_ context.Context, documents []embed.DocumentInput) ([][][]float32, error) {
	result := make([][][]float32, len(documents))
	for i, document := range documents {
		result[i] = make([][]float32, len(document.Chunks))
		for j, chunk := range document.Chunks {
			result[i][j] = make([]float32, c.dimension)
			if strings.Contains(chunk, "Confirmed action") {
				result[i][j][0] = 1
			} else {
				result[i][j][1] = 1
			}
		}
	}
	return result, c.err
}

func (c *fixedDimensionEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	result := make([]float32, c.dimension)
	result[0] = 1
	return result, c.err
}

func (c *fixedDimensionEmbedder) EmbedDocuments(_ context.Context, documents []embed.DocumentInput) ([][][]float32, error) {
	result := make([][][]float32, len(documents))
	for i, document := range documents {
		result[i] = make([][]float32, len(document.Chunks))
		for j := range document.Chunks {
			result[i][j] = make([]float32, c.dimension)
			result[i][j][0] = 1
		}
	}
	return result, c.err
}

func (c *batchRecordingEmbedder) EmbedDocuments(_ context.Context, documents []embed.DocumentInput) ([][][]float32, error) {
	c.batchSizes = append(c.batchSizes, len(documents))
	result := make([][][]float32, len(documents))
	for i, document := range documents {
		result[i] = make([][]float32, len(document.Chunks))
		for j := range document.Chunks {
			result[i][j] = make([]float32, max(c.dimension, 1))
			result[i][j][0] = float32(c.next)
		}
		c.next++
	}
	return result, nil
}

func (c *countingEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	c.queryCalls++
	return append([]float32(nil), c.queryVector...), nil
}
