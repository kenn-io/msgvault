//go:build sqlite_vec || pgvector

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

func TestDocumentVectorLedgerCommandsNeverOpenRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, spec := documentVectorCommandFixture(t)
	t.Setenv("SYNTHETIC_EMBEDDING_KEY", "secret-that-must-not-print")
	runtimeCalls := 0
	deps := documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		runDocumentVector: func(context.Context, *store.Store, int64, int) (vectordocument.ReconcileResult, error) {
			runtimeCalls++
			return vectordocument.ReconcileResult{}, nil
		},
	}

	unconfirmed := newDocumentsCmd(deps)
	var unconfirmedOutput bytes.Buffer
	unconfirmed.SetOut(&unconfirmedOutput)
	unconfirmed.SetArgs([]string{documentVectorsSubcommand, "consent"})
	require.ErrorContains(unconfirmed.ExecuteContext(t.Context()), "--yes")
	assert.Contains(unconfirmedOutput.String(), "Hosted document embedding disclosure:")
	assert.Contains(unconfirmedOutput.String(), "Destination: https://embeddings.example.test/v1")
	assert.Contains(unconfirmedOutput.String(), "Authentication: environment variable SYNTHETIC_EMBEDDING_KEY")
	assert.Contains(unconfirmedOutput.String(), "API format: openai")
	assert.Contains(unconfirmedOutput.String(), "Model: embed-test")
	assert.Contains(unconfirmedOutput.String(), "Dimension: 3")
	assert.Contains(unconfirmedOutput.String(), "Maximum input: 4096 characters")
	assert.Contains(unconfirmedOutput.String(), "Docbank-prepared normalized attachment document inputs will be sent")
	assert.NotContains(unconfirmedOutput.String(), "Explicit semantic or hybrid document searches")
	assert.NotContains(unconfirmedOutput.String(), "secret-that-must-not-print")
	consentSpec, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(err)
	unconfirmedConsent, err := fixture.Store.GetDocumentVectorConsent(t.Context(), consentSpec.EgressFingerprint)
	require.NoError(err)
	assert.Nil(unconfirmedConsent)

	consent := newDocumentsCmd(deps)
	var consentOutput bytes.Buffer
	consent.SetOut(&consentOutput)
	consent.SetArgs([]string{documentVectorsSubcommand, "consent", "--yes"})
	require.NoError(consent.ExecuteContext(t.Context()))
	assert.Contains(consentOutput.String(), "Hosted document embedding disclosure:")
	assert.Contains(consentOutput.String(), "Recorded consent for document vector egress fingerprint "+consentSpec.EgressFingerprint)
	assert.NotContains(consentOutput.String(), "secret-that-must-not-print")
	recorded, err := fixture.Store.GetDocumentVectorConsent(t.Context(), consentSpec.EgressFingerprint)
	require.NoError(err)
	require.NotNil(recorded)
	assert.Equal(spec, recorded.DocumentVectorGenerationSpec)
	assert.Equal("document_embedding", recorded.Purpose)

	queryConsentSpec, err := configuredDocumentVectorQueryConsentSpec(spec)
	require.NoError(err)
	assert.NotEqual(consentSpec.EgressFingerprint, queryConsentSpec.EgressFingerprint)
	queryConsent := newDocumentsCmd(deps)
	var queryConsentOutput bytes.Buffer
	queryConsent.SetOut(&queryConsentOutput)
	queryConsent.SetArgs([]string{documentVectorsSubcommand, "consent", "--purpose", "queries", "--yes"})
	require.NoError(queryConsent.ExecuteContext(t.Context()))
	assert.Contains(queryConsentOutput.String(), "Explicit semantic or hybrid document searches will send query text")
	recordedQuery, err := fixture.Store.GetDocumentVectorConsent(t.Context(), queryConsentSpec.EgressFingerprint)
	require.NoError(err)
	require.NotNil(recordedQuery)
	assert.Equal("query_embedding", recordedQuery.Purpose)

	consentedEndpoint := cfg.Vector.Embeddings.Endpoint
	cfg.Vector.Embeddings.Endpoint = "https://hosted.example.test/v1"
	changedConsentSpec, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(err)
	assert.NotEqual(consentSpec.EgressFingerprint, changedConsentSpec.EgressFingerprint)
	require.ErrorContains(requireDocumentVectorConsent(t.Context(), fixture.Store, spec), "not consented")
	cfg.Vector.Embeddings.Endpoint = consentedEndpoint
	require.NoError(requireDocumentVectorConsent(t.Context(), fixture.Store, spec))

	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	status := newDocumentsCmd(deps)
	var statusOutput bytes.Buffer
	status.SetOut(&statusOutput)
	status.SetArgs([]string{documentVectorsSubcommand, statusValue})
	require.NoError(status.ExecuteContext(t.Context()))
	assert.Contains(statusOutput.String(), "building_generation=")
	assert.Contains(statusOutput.String(), "state=building")
	assert.Contains(statusOutput.String(), "pending=0 retryable=0 terminal=0 ready_live=0 obsolete=0 cleanup_pending=0")
	assert.Contains(statusOutput.String(), "coverage_required=0 coverage_ready=0")

	retry := newDocumentsCmd(deps)
	retry.SetOut(&bytes.Buffer{})
	retry.SetArgs([]string{documentVectorsSubcommand, "retry", "--generation-id", "999", "--limit", "1"})
	require.Error(retry.ExecuteContext(t.Context()))

	retire := newDocumentsCmd(deps)
	retire.SetOut(&bytes.Buffer{})
	retire.SetArgs([]string{documentVectorsSubcommand, cliEmbeddingsOperationRetire, "--generation-id", fmtInt64(generation.ID), "--yes"})
	require.NoError(retire.ExecuteContext(t.Context()))
	assert.Zero(runtimeCalls)
}

func TestDocumentVectorStatusWorksWhenEmbeddingsAreDisabled(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = config.NewDefaultConfig()
	runtimeCalls := 0
	command := newDocumentsCmd(documentsCommandDeps{
		runDocumentVector: func(context.Context, *store.Store, int64, int) (vectordocument.ReconcileResult, error) {
			runtimeCalls++
			return vectordocument.ReconcileResult{}, nil
		},
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{documentVectorsSubcommand, statusValue, "--json"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.JSONEq(t, `{"enabled":false}`, output.String())
	assert.Zero(t, runtimeCalls)
}

func TestConfiguredDocumentVectorSpecRejectsInvalidDisabledEmbeddingPolicy(t *testing.T) {
	fixture, _ := documentVectorCommandFixture(t)
	base := *cfg
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{
			name: "endpoint",
			mutate: func(c *config.Config) {
				c.Vector.Embeddings.Endpoint = "::not-a-url"
			},
			wantErr: "endpoint",
		},
		{
			name: "dimension",
			mutate: func(c *config.Config) {
				c.Vector.Embeddings.Dimension = 0
			},
			wantErr: "dimension",
		},
		{
			name: "input limit",
			mutate: func(c *config.Config) {
				c.Vector.Embeddings.MaxInputChars = -1
			},
			wantErr: "max_input_chars",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			changed.Vector.Enabled = false
			test.mutate(&changed)
			cfg = &changed
			var err error
			require.NotPanics(t, func() {
				_, err = configuredDocumentVectorSpec(t.Context(), fixture.Store)
			})
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestDocumentVectorStatusWorksBeforeExtractionTargetExists(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = config.NewDefaultConfig()
	cfg.Vector.Enabled = true
	cfg.Vector.Embeddings.Endpoint = "https://embeddings.example.test/v1"
	cfg.Vector.Embeddings.Model = "embed-test"
	cfg.Vector.Embeddings.Dimension = 3
	cfg.Attachments.Documents.Index.Embeddings.Enabled = true
	fixture := storetest.New(t)
	command := newDocumentsCmd(documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{documentVectorsSubcommand, statusValue, "--json"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.JSONEq(t, `{"enabled":true,"configured":false}`, output.String())
}

func TestDocumentVectorProviderCommandsUseRuntimeAndValidateBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, spec := documentVectorCommandFixture(t)
	consentSpec, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(err)
	_, _, err = fixture.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
	require.NoError(err)
	var calls []int64
	deps := documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		runDocumentVector: func(_ context.Context, _ *store.Store, generationID int64, _ int) (vectordocument.ReconcileResult, error) {
			calls = append(calls, generationID)
			return vectordocument.ReconcileResult{}, nil
		},
	}

	invalid := newDocumentsCmd(deps)
	invalid.SetArgs([]string{documentVectorsSubcommand, documentBuildSubcommand, "--limit", "0"})
	require.ErrorContains(invalid.ExecuteContext(t.Context()), "limit")
	assert.Empty(calls)

	build := newDocumentsCmd(deps)
	build.SetOut(&bytes.Buffer{})
	build.SetArgs([]string{documentVectorsSubcommand, documentBuildSubcommand, "--limit", "1"})
	require.NoError(build.ExecuteContext(t.Context()))
	require.Len(calls, 1)
	building, err := fixture.Store.GetBuildingDocumentVectorGeneration(t.Context())
	require.NoError(err)
	require.NotNil(building)

	resume := newDocumentsCmd(deps)
	resume.SetOut(&bytes.Buffer{})
	resume.SetArgs([]string{documentVectorsSubcommand, cmdUseResume, "--generation-id", fmtInt64(building.ID), "--limit", "1"})
	require.NoError(resume.ExecuteContext(t.Context()))
	assert.Len(calls, 2)

	require.NoError(fixture.Store.ActivateDocumentVectorGeneration(t.Context(), building.ID, time.Now()))
	rebuild := newDocumentsCmd(deps)
	rebuild.SetOut(&bytes.Buffer{})
	rebuild.SetArgs([]string{documentVectorsSubcommand, "rebuild", "--generation-id", fmtInt64(building.ID), "--limit", "1", "--yes"})
	require.NoError(rebuild.ExecuteContext(t.Context()))
	assert.Len(calls, 3)
}

func TestDocumentVectorResumeRunsBoundedCleanupForRetiredGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, spec := documentVectorCommandFixture(t)
	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	token := strings.Repeat("8", 64)
	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
		INSERT INTO document_vector_publications
			(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
			 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), generation.ID,
		"manual-cleanup-extraction", spec.TargetExtractionProfileID, strings.Repeat("a", 64),
		"original", 1, "manual-cleanup-chunk", "manual-cleanup-checksum", 1, token)
	require.NoError(err)
	retired, err := fixture.Store.RetireDocumentVectorGeneration(t.Context(), generation.ID, time.Now())
	require.NoError(err)
	require.True(retired)

	backend := &commandDocumentVectorBackend{}
	deps := documentsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		runDocumentVector: func(ctx context.Context, st *store.Store, generationID int64, limit int) (vectordocument.ReconcileResult, error) {
			return runDocumentVectorWithFeatures(ctx, st, &vectorFeatures{
				DocumentBackend: backend,
				Cfg:             cfg.Vector,
			}, generationID, limit, testOperationPassScope("document-vector:retired-cleanup"))
		},
	}
	command := newDocumentsCmd(deps)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{
		documentVectorsSubcommand, cmdUseResume,
		"--generation-id", fmtInt64(generation.ID), "--limit", "1",
	})

	require.NoError(command.ExecuteContext(t.Context()))
	var result vectordocument.ReconcileResult
	require.NoError(json.Unmarshal(output.Bytes(), &result))
	assert.True(result.Purged)
	assert.True(result.Converged)
	assert.Equal([][]string{{token}}, backend.deletes)
	_, err = fixture.Store.GetDocumentVectorGeneration(t.Context(), generation.ID)
	require.ErrorContains(err, "not found")
}

func TestDocumentVectorWorkerCheckpointsPartialErrorResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	wantErr := errors.New("provider partial failure")
	fixture := storetest.New(t)
	runResult := vectordocument.RunResult{
		Attempted: 3, Succeeded: 2, Failed: 1,
		ProviderCalls: 1, ProviderDocuments: 2, ProviderChunks: 3, ProviderInputChars: 44,
		AfterGenerationID: 7, AfterChunkID: 81,
	}
	worker := checkpointingDocumentVectorWorker{
		worker:       fakeDocumentVectorWorkerRunner{result: runResult, err: wantErr},
		checkpointer: &fakeDocumentVectorCheckpointer{},
		recorder:     fixture.Store,
		scope:        testOperationPassScope("document-vector:resume"),
		fingerprint:  strings.Repeat("a", 64),
		now:          func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) },
	}
	result, err := worker.Run(t.Context(), 7, 10)
	require.ErrorIs(err, wantErr)
	assert.Equal(runResult, result)
	checkpoint, ok := worker.checkpointer.(*fakeDocumentVectorCheckpointer)
	require.True(ok)
	require.Len(checkpoint.calls, 1)
	assert.Equal(strings.Repeat("a", 64), checkpoint.calls[0].fingerprint)
	assert.Equal(int64(81), checkpoint.calls[0].afterChunkID)
	assert.False(checkpoint.calls[0].exhausted)
	assert.Equal(store.DocumentVectorUsageDelta{
		ProviderCalls: 1, ProviderDocuments: 2, ProviderChunks: 3, ProviderInputChars: 44,
	}, checkpoint.calls[0].delta)
	runs := operationRunsForKind(t, fixture.Store, operations.KindDocumentEmbedding)
	require.Len(runs, 1)
	assert.Equal(operations.StatePartial, runs[0].State)
	assert.Equal([]operations.PublicCounter{
		{Name: operations.CounterAttempted, Unit: operations.CounterUnitChunks, Value: 3},
		{Name: operations.CounterSucceeded, Unit: operations.CounterUnitChunks, Value: 2},
		{Name: operations.CounterFailed, Unit: operations.CounterUnitChunks, Value: 1},
	}, runs[0].Counters)
}

func TestDocumentVectorTerminalReplayReturnsFixedFailure(t *testing.T) {
	started := time.Date(2026, 8, 30, 15, 45, 0, 0, time.UTC)
	finished := started.Add(time.Second)
	trigger := operations.TriggerManual
	id, err := operations.NewInt64ID(operations.KindDocumentEmbedding, 11)
	require.NoError(t, err)
	run := &operations.Run{
		ID: id, Lane: operations.LaneDocuments, State: operations.StateFailed, Trigger: &trigger,
		StartedAt: started, FinishedAt: &finished,
		Counters: operations.InvocationCounters{Attempted: 1, Failed: 1}.
			PublicCounters(operations.KindDocumentEmbedding),
		Error: operations.FixedPublicError(operations.PublicErrorInvocationTimeout),
	}

	result, replayErr := documentVectorRunResultFromOperationRun(run)

	assert.Equal(t, 1, result.Attempted)
	assert.Equal(t, 1, result.Failed)
	require.ErrorIs(t, replayErr, context.DeadlineExceeded)
	assert.Equal(t, "Operation timed out.", replayErr.Error())
}

func TestDocumentVectorWorkerOwnersAreUniqueWithinOneProcess(t *testing.T) {
	first := nextDocumentVectorWorkerOwner()
	second := nextDocumentVectorWorkerOwner()
	assert.NotEqual(t, first, second)
	assert.True(t, strings.HasPrefix(first, "document-vector-"))
	_, err := uuid.Parse(strings.TrimPrefix(first, "document-vector-"))
	require.NoError(t, err)
}

type fakeDocumentVectorWorkerRunner struct {
	result vectordocument.RunResult
	err    error
}

func (r fakeDocumentVectorWorkerRunner) Run(context.Context, vectordocument.GenerationID, int) (vectordocument.RunResult, error) {
	return r.result, r.err
}

type fakeDocumentVectorCheckpoint struct {
	afterChunkID int64
	exhausted    bool
	fingerprint  string
	delta        store.DocumentVectorUsageDelta
}

type fakeDocumentVectorCheckpointer struct {
	calls []fakeDocumentVectorCheckpoint
}

func (c *fakeDocumentVectorCheckpointer) CheckpointDocumentVectorBuildForFingerprint(_ context.Context, _ int64, fingerprint string, afterChunkID int64, exhausted bool, delta store.DocumentVectorUsageDelta, _ time.Time) error {
	c.calls = append(c.calls, fakeDocumentVectorCheckpoint{
		fingerprint: fingerprint, afterChunkID: afterChunkID, exhausted: exhausted, delta: delta,
	})
	return nil
}

func TestScheduledDocumentVectorRotationRetiresObsoleteBuildingBeforeDesiredBuild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, desired := documentVectorCommandFixture(t)
	consentSpec, err := configuredDocumentVectorConsentSpec(desired)
	require.NoError(err)
	_, _, err = fixture.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
	require.NoError(err)
	activeSpec := desired
	activeSpec.Fingerprint = strings.Repeat("1", 64)
	active, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), activeSpec)
	require.NoError(err)
	require.NoError(fixture.Store.ActivateDocumentVectorGeneration(t.Context(), active.ID, time.Now()))
	obsoleteSpec := desired
	obsoleteSpec.Fingerprint = strings.Repeat("2", 64)
	obsolete, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), obsoleteSpec)
	require.NoError(err)
	for index := range 3 {
		_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
			INSERT INTO document_vector_publications
				(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
				 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), obsolete.ID,
			fmt.Sprintf("obsolete-%d", index), desired.TargetExtractionProfileID, strings.Repeat("a", 64),
			"original", index+1, fmt.Sprintf("chunk-%d", index), fmt.Sprintf("checksum-%d", index),
			1, fmt.Sprintf("%064x", index+1))
		require.NoError(err)
	}
	client := &commandDocumentSemanticClient{}
	backend := &commandDocumentVectorBackend{}
	vf := &vectorFeatures{
		DocumentBackend: backend, SemanticClient: client, Cfg: cfg.Vector,
	}

	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 2))
	stillActive, err := fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(err)
	require.NotNil(stillActive)
	assert.Equal(active.ID, stillActive.ID)
	retired, err := fixture.Store.GetDocumentVectorGeneration(t.Context(), obsolete.ID)
	require.NoError(err)
	assert.Equal(store.DocumentVectorGenerationRetired, retired.State)
	require.Len(backend.deletes, 1)
	assert.Len(backend.deletes[0], 2)

	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 2))
	_, err = fixture.Store.GetDocumentVectorGeneration(t.Context(), obsolete.ID)
	require.ErrorContains(err, "not found")
	require.Len(backend.deletes, 2)
	assert.Len(backend.deletes[1], 1)
	stillActive, err = fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(err)
	assert.Equal(active.ID, stillActive.ID)

	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 2))
	newActive, err := fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(err)
	require.NotNil(newActive)
	assert.Equal(desired, newActive.DocumentVectorGenerationSpec)
	assert.Zero(client.documentCalls)
}

func TestScheduledDocumentVectorCleansRetiredWithoutConsentOrProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, spec := documentVectorCommandFixture(t)
	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(err)
	token := strings.Repeat("9", 64)
	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
		INSERT INTO document_vector_publications
			(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
			 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), generation.ID,
		"retired-extraction", spec.TargetExtractionProfileID, strings.Repeat("a", 64),
		"original", 1, "retired-chunk", "retired-checksum", 1, token)
	require.NoError(err)
	retired, err := fixture.Store.RetireDocumentVectorGeneration(t.Context(), generation.ID, time.Now())
	require.NoError(err)
	require.True(retired)
	backend := &commandDocumentVectorBackend{}
	vf := &vectorFeatures{DocumentBackend: backend, Cfg: cfg.Vector}

	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))

	assert.Equal([][]string{{token}}, backend.deletes)
	_, err = fixture.Store.GetDocumentVectorGeneration(t.Context(), generation.ID)
	require.ErrorContains(err, "not found")
	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))
}

func TestScheduledDocumentVectorObservesConsentRecordedAfterRuntimeInitialization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, spec := documentVectorCommandFixture(t)
	vf := &vectorFeatures{
		DocumentBackend: &commandDocumentVectorBackend{},
		SemanticClient:  &commandDocumentSemanticClient{},
		Cfg:             cfg.Vector,
	}

	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))
	active, err := fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(err)
	assert.Nil(active)

	consentSpec, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(err)
	_, _, err = fixture.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
	require.NoError(err)

	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))
	active, err = fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(err)
	require.NotNil(active)
	assert.Equal(spec, active.DocumentVectorGenerationSpec)
}

func TestScheduledDocumentVectorCleansObsoleteActiveTokensAfterCoverageIsComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, desired := documentVectorCommandFixture(t)
	consentSpec, err := configuredDocumentVectorConsentSpec(desired)
	require.NoError(err)
	_, _, err = fixture.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
	require.NoError(err)
	active, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), desired)
	require.NoError(err)
	require.NoError(fixture.Store.ActivateDocumentVectorGeneration(t.Context(), active.ID, time.Now()))
	token := strings.Repeat("c", 64)
	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
		INSERT INTO document_vector_publications
			(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
			 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), active.ID,
		"deleted-extraction", desired.TargetExtractionProfileID, strings.Repeat("d", 64),
		"original", 1, "deleted-chunk", "deleted-checksum", 1, token)
	require.NoError(err)
	coverage, err := fixture.Store.GetDocumentVectorCoverage(t.Context(), active.ID)
	require.NoError(err)
	assert.True(coverage.Complete())
	status, err := fixture.Store.GetDocumentVectorGenerationStatus(t.Context(), active.ID, "", 10)
	require.NoError(err)
	assert.Equal(int64(1), status.CleanupPending)

	client := &commandDocumentSemanticClient{}
	backend := &commandDocumentVectorBackend{}
	vf := &vectorFeatures{
		DocumentBackend: backend, SemanticClient: client, Cfg: cfg.Vector,
	}
	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))
	require.Equal([][]string{{token}}, backend.deletes)
	assert.Zero(client.documentCalls)
	status, err = fixture.Store.GetDocumentVectorGenerationStatus(t.Context(), active.ID, "", 10)
	require.NoError(err)
	assert.Zero(status.CleanupPending)
	var publications int
	require.NoError(fixture.Store.DB().QueryRow(fixture.Store.Rebind(
		`SELECT COUNT(*) FROM document_vector_publications WHERE generation_id = ?`), active.ID).Scan(&publications))
	assert.Zero(publications)

	require.NoError(runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))
	assert.Len(backend.deletes, 1, "a converged replay does not re-delete finalized tokens")
	assert.Zero(client.documentCalls)
}

type commandDocumentSemanticClient struct{ documentCalls int }

func (*commandDocumentSemanticClient) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

func (c *commandDocumentSemanticClient) EmbedDocuments(context.Context, []vector.DocumentInput) ([][][]float32, error) {
	c.documentCalls++
	return nil, nil
}

type commandDocumentVectorBackend struct{ deletes [][]string }

func (*commandDocumentVectorBackend) PutUnpublished(context.Context, vectordocument.GenerationID, int, []vectordocument.Embedding) error {
	return nil
}

func (b *commandDocumentVectorBackend) DeleteTokens(_ context.Context, _ vectordocument.GenerationID, tokens []string) error {
	b.deletes = append(b.deletes, append([]string(nil), tokens...))
	return nil
}

func (*commandDocumentVectorBackend) Search(context.Context, vectordocument.GenerationID, int, []float32, int) ([]vectordocument.Hit, error) {
	return nil, nil
}

func documentVectorCommandFixture(t *testing.T) (*storetest.Fixture, store.DocumentVectorGenerationSpec) {
	t.Helper()
	markDaemonCLISubprocessForTest(t)
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "https://embeddings.example.test/v1"
	c.Vector.Embeddings.APIKeyEnv = "SYNTHETIC_EMBEDDING_KEY"
	c.Vector.Embeddings.Model = "embed-test"
	c.Vector.Embeddings.Dimension = 3
	c.Vector.Embeddings.MaxInputChars = 4096
	c.Attachments.Documents.Index.Embeddings.Enabled = true
	c.Attachments.Documents.Index.Embeddings.Profile = "vector.embeddings"
	cfg = c
	fixture := storetest.New(t)
	fingerprint := strings.Repeat("7", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint, Provider: "synthetic",
		Endpoint: "https://documents.example.test/v1", Region: localValue, Model: "extract-test",
		RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"application/pdf"}, PolicyJSON: []byte(`{"policy":1}`),
	}
	_, err := fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(t, err)
	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), profile.ID)
	require.NoError(t, err)
	spec, err := desiredDocumentVectorSpec(t.Context(), fixture.Store)
	require.NoError(t, err)
	return fixture, spec
}

func fmtInt64(value int64) string { return strconv.FormatInt(value, 10) }
