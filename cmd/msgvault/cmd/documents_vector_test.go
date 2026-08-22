//go:build sqlite_vec || pgvector

package cmd

import (
	"bytes"
	"context"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

func TestDocumentVectorLedgerCommandsNeverOpenRuntime(t *testing.T) {
	fixture, spec := documentVectorCommandFixture(t)
	t.Setenv("SYNTHETIC_EMBEDDING_KEY", "secret-that-must-not-print")
	cfg.Vector.Embeddings.Endpoint = "https://endpoint-user:endpoint-secret@embeddings.example.test/v1?api_key=query-secret#fragment-secret"
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
	require.ErrorContains(t, unconfirmed.ExecuteContext(t.Context()), "--yes")
	assert.Contains(t, unconfirmedOutput.String(), "Hosted document embedding disclosure:")
	assert.Contains(t, unconfirmedOutput.String(), "Destination: https://embeddings.example.test/v1")
	assert.Contains(t, unconfirmedOutput.String(), "Authentication: environment variable SYNTHETIC_EMBEDDING_KEY")
	assert.Contains(t, unconfirmedOutput.String(), "API format: openai")
	assert.Contains(t, unconfirmedOutput.String(), "Model: embed-test")
	assert.Contains(t, unconfirmedOutput.String(), "Dimension: 3")
	assert.Contains(t, unconfirmedOutput.String(), "Maximum input: 4096 characters")
	assert.Contains(t, unconfirmedOutput.String(), "Normalized attachment document chunk text will be sent")
	assert.Contains(t, unconfirmedOutput.String(), "Explicit semantic or hybrid document searches will send query text")
	assert.NotContains(t, unconfirmedOutput.String(), "secret-that-must-not-print")
	assert.NotContains(t, unconfirmedOutput.String(), "endpoint-user")
	assert.NotContains(t, unconfirmedOutput.String(), "endpoint-secret")
	assert.NotContains(t, unconfirmedOutput.String(), "query-secret")
	assert.NotContains(t, unconfirmedOutput.String(), "fragment-secret")
	consentSpec, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(t, err)
	unconfirmedConsent, err := fixture.Store.GetDocumentVectorConsent(t.Context(), consentSpec.EgressFingerprint)
	require.NoError(t, err)
	assert.Nil(t, unconfirmedConsent)

	consent := newDocumentsCmd(deps)
	var consentOutput bytes.Buffer
	consent.SetOut(&consentOutput)
	consent.SetArgs([]string{documentVectorsSubcommand, "consent", "--yes"})
	require.NoError(t, consent.ExecuteContext(t.Context()))
	assert.Contains(t, consentOutput.String(), "Hosted document embedding disclosure:")
	assert.Contains(t, consentOutput.String(), "Recorded consent for document vector egress fingerprint "+consentSpec.EgressFingerprint)
	assert.NotContains(t, consentOutput.String(), "secret-that-must-not-print")
	recorded, err := fixture.Store.GetDocumentVectorConsent(t.Context(), consentSpec.EgressFingerprint)
	require.NoError(t, err)
	require.NotNil(t, recorded)
	assert.Equal(t, spec, recorded.DocumentVectorGenerationSpec)

	consentedEndpoint := cfg.Vector.Embeddings.Endpoint
	cfg.Vector.Embeddings.Endpoint = "https://hosted.example.test/v1"
	changedConsentSpec, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(t, err)
	assert.NotEqual(t, consentSpec.EgressFingerprint, changedConsentSpec.EgressFingerprint)
	require.ErrorContains(t, requireDocumentVectorConsent(t.Context(), fixture.Store, spec), "not consented")
	cfg.Vector.Embeddings.Endpoint = consentedEndpoint
	require.NoError(t, requireDocumentVectorConsent(t.Context(), fixture.Store, spec))

	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), spec)
	require.NoError(t, err)
	status := newDocumentsCmd(deps)
	var statusOutput bytes.Buffer
	status.SetOut(&statusOutput)
	status.SetArgs([]string{documentVectorsSubcommand, statusValue})
	require.NoError(t, status.ExecuteContext(t.Context()))
	assert.Contains(t, statusOutput.String(), "building_generation=")
	assert.Contains(t, statusOutput.String(), "state=building")
	assert.Contains(t, statusOutput.String(), "pending=0 retryable=0 terminal=0 ready_live=0 obsolete=0 cleanup_pending=0")
	assert.Contains(t, statusOutput.String(), "coverage_required=0 coverage_ready=0")

	retry := newDocumentsCmd(deps)
	retry.SetOut(&bytes.Buffer{})
	retry.SetArgs([]string{documentVectorsSubcommand, "retry", "--generation-id", "999", "--limit", "1"})
	require.Error(t, retry.ExecuteContext(t.Context()))

	retire := newDocumentsCmd(deps)
	retire.SetOut(&bytes.Buffer{})
	retire.SetArgs([]string{documentVectorsSubcommand, cliEmbeddingsOperationRetire, "--generation-id", fmtInt64(generation.ID), "--yes"})
	require.NoError(t, retire.ExecuteContext(t.Context()))
	assert.Zero(t, runtimeCalls)
}

func TestDocumentVectorStatusWorksWhenEmbeddingsAreDisabled(t *testing.T) {
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

func TestDocumentVectorStatusWorksBeforeExtractionTargetExists(t *testing.T) {
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
	fixture, spec := documentVectorCommandFixture(t)
	consentSpec, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(t, err)
	_, _, err = fixture.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
	require.NoError(t, err)
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
	require.ErrorContains(t, invalid.ExecuteContext(t.Context()), "limit")
	assert.Empty(t, calls)

	build := newDocumentsCmd(deps)
	build.SetOut(&bytes.Buffer{})
	build.SetArgs([]string{documentVectorsSubcommand, documentBuildSubcommand, "--limit", "1"})
	require.NoError(t, build.ExecuteContext(t.Context()))
	require.Len(t, calls, 1)
	building, err := fixture.Store.GetBuildingDocumentVectorGeneration(t.Context())
	require.NoError(t, err)
	require.NotNil(t, building)

	resume := newDocumentsCmd(deps)
	resume.SetOut(&bytes.Buffer{})
	resume.SetArgs([]string{documentVectorsSubcommand, cmdUseResume, "--generation-id", fmtInt64(building.ID), "--limit", "1"})
	require.NoError(t, resume.ExecuteContext(t.Context()))
	assert.Len(t, calls, 2)

	require.NoError(t, fixture.Store.ActivateDocumentVectorGeneration(t.Context(), building.ID, time.Now()))
	rebuild := newDocumentsCmd(deps)
	rebuild.SetOut(&bytes.Buffer{})
	rebuild.SetArgs([]string{documentVectorsSubcommand, "rebuild", "--generation-id", fmtInt64(building.ID), "--limit", "1", "--yes"})
	require.NoError(t, rebuild.ExecuteContext(t.Context()))
	assert.Len(t, calls, 3)
}

func TestDocumentVectorWorkerCheckpointsPartialErrorResult(t *testing.T) {
	wantErr := errors.New("provider partial failure")
	runResult := vectordocument.RunResult{
		ProviderCalls: 1, ProviderDocuments: 2, ProviderChunks: 3, ProviderInputChars: 44,
		AfterGenerationID: 7, AfterChunkID: 81,
	}
	worker := checkpointingDocumentVectorWorker{
		worker:       fakeDocumentVectorWorkerRunner{result: runResult, err: wantErr},
		checkpointer: &fakeDocumentVectorCheckpointer{},
		fingerprint:  strings.Repeat("a", 64),
		now:          func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) },
	}
	result, err := worker.Run(t.Context(), 7, 10)
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, runResult, result)
	checkpoint, ok := worker.checkpointer.(*fakeDocumentVectorCheckpointer)
	require.True(t, ok)
	require.Len(t, checkpoint.calls, 1)
	assert.Equal(t, strings.Repeat("a", 64), checkpoint.calls[0].fingerprint)
	assert.Equal(t, int64(81), checkpoint.calls[0].afterChunkID)
	assert.False(t, checkpoint.calls[0].exhausted)
	assert.Equal(t, store.DocumentVectorUsageDelta{
		ProviderCalls: 1, ProviderDocuments: 2, ProviderChunks: 3, ProviderInputChars: 44,
	}, checkpoint.calls[0].delta)
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
	fixture, desired := documentVectorCommandFixture(t)
	consentSpec, err := configuredDocumentVectorConsentSpec(desired)
	require.NoError(t, err)
	_, _, err = fixture.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
	require.NoError(t, err)
	activeSpec := desired
	activeSpec.Fingerprint = strings.Repeat("1", 64)
	active, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), activeSpec)
	require.NoError(t, err)
	require.NoError(t, fixture.Store.ActivateDocumentVectorGeneration(t.Context(), active.ID, time.Now()))
	obsoleteSpec := desired
	obsoleteSpec.Fingerprint = strings.Repeat("2", 64)
	obsolete, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), obsoleteSpec)
	require.NoError(t, err)
	for index := range 3 {
		_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
			INSERT INTO document_vector_publications
				(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
				 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), obsolete.ID,
			fmt.Sprintf("obsolete-%d", index), desired.TargetExtractionProfileID, strings.Repeat("a", 64),
			"original", index+1, fmt.Sprintf("chunk-%d", index), fmt.Sprintf("checksum-%d", index),
			1, fmt.Sprintf("%064x", index+1))
		require.NoError(t, err)
	}
	client := &commandDocumentSemanticClient{}
	backend := &commandDocumentVectorBackend{}
	vf := &vectorFeatures{DocumentBackend: backend, SemanticClient: client, Cfg: cfg.Vector}

	require.NoError(t, runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 2))
	stillActive, err := fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(t, err)
	require.NotNil(t, stillActive)
	assert.Equal(t, active.ID, stillActive.ID)
	retired, err := fixture.Store.GetDocumentVectorGeneration(t.Context(), obsolete.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DocumentVectorGenerationRetired, retired.State)
	require.Len(t, backend.deletes, 1)
	assert.Len(t, backend.deletes[0], 2)

	require.NoError(t, runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 2))
	_, err = fixture.Store.GetDocumentVectorGeneration(t.Context(), obsolete.ID)
	require.ErrorContains(t, err, "not found")
	require.Len(t, backend.deletes, 2)
	assert.Len(t, backend.deletes[1], 1)
	stillActive, err = fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, active.ID, stillActive.ID)

	require.NoError(t, runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 2))
	newActive, err := fixture.Store.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(t, err)
	require.NotNil(t, newActive)
	assert.Equal(t, desired, newActive.DocumentVectorGenerationSpec)
	assert.Zero(t, client.documentCalls)
}

func TestScheduledDocumentVectorCleansObsoleteActiveTokensAfterCoverageIsComplete(t *testing.T) {
	fixture, desired := documentVectorCommandFixture(t)
	consentSpec, err := configuredDocumentVectorConsentSpec(desired)
	require.NoError(t, err)
	_, _, err = fixture.Store.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
	require.NoError(t, err)
	active, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), desired)
	require.NoError(t, err)
	require.NoError(t, fixture.Store.ActivateDocumentVectorGeneration(t.Context(), active.ID, time.Now()))
	token := strings.Repeat("c", 64)
	_, err = fixture.Store.DB().Exec(fixture.Store.Rebind(`
		INSERT INTO document_vector_publications
			(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
			 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence, token, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')`), active.ID,
		"deleted-extraction", desired.TargetExtractionProfileID, strings.Repeat("d", 64),
		"original", 1, "deleted-chunk", "deleted-checksum", 1, token)
	require.NoError(t, err)
	coverage, err := fixture.Store.GetDocumentVectorCoverage(t.Context(), active.ID)
	require.NoError(t, err)
	assert.True(t, coverage.Complete())
	status, err := fixture.Store.GetDocumentVectorGenerationStatus(t.Context(), active.ID, "", 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.CleanupPending)

	client := &commandDocumentSemanticClient{}
	backend := &commandDocumentVectorBackend{}
	vf := &vectorFeatures{DocumentBackend: backend, SemanticClient: client, Cfg: cfg.Vector}
	require.NoError(t, runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))
	require.Equal(t, [][]string{{token}}, backend.deletes)
	assert.Zero(t, client.documentCalls)
	status, err = fixture.Store.GetDocumentVectorGenerationStatus(t.Context(), active.ID, "", 10)
	require.NoError(t, err)
	assert.Zero(t, status.CleanupPending)
	var publications int
	require.NoError(t, fixture.Store.DB().QueryRow(fixture.Store.Rebind(
		`SELECT COUNT(*) FROM document_vector_publications WHERE generation_id = ?`), active.ID).Scan(&publications))
	assert.Zero(t, publications)

	require.NoError(t, runScheduledDocumentVectorGeneration(t.Context(), fixture.Store, vf, 10))
	assert.Len(t, backend.deletes, 1, "a converged replay does not re-delete finalized tokens")
	assert.Zero(t, client.documentCalls)
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
