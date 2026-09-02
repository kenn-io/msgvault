package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

type cursorOnlyAssemblySource struct{ *store.Store }

func (cursorOnlyAssemblySource) LoadPersonFactState(
	context.Context, int64, personfacts.Catalog,
) (peoplesweep.PersonFactState, error) {
	return peoplesweep.PersonFactState{}, nil
}

type noProviderSweepRunner struct{ calls int }

func (r *noProviderSweepRunner) PrepareStructured(context.Context, peoplesweep.StructuredRequest) (peoplesweep.PreparedStructuredRequest, error) {
	r.calls++
	return peoplesweep.PreparedStructuredRequest{}, errors.New("cursor-only sweep must not prepare provider work")
}
func (r *noProviderSweepRunner) PrepareRepair(peoplesweep.StructuredRequest, peoplesweep.ValidationFailure) (peoplesweep.PreparedStructuredRequest, error) {
	r.calls++
	return peoplesweep.PreparedStructuredRequest{}, errors.New("cursor-only sweep must not prepare provider repair work")
}

func (r *noProviderSweepRunner) BeginStructuredExecution(context.Context, peoplesweep.PreparedStructuredRequest) (peoplesweep.StructuredExecutionSession, error) {
	r.calls++
	return nil, errors.New("cursor-only sweep must not begin provider execution")
}

func (r *noProviderSweepRunner) RunPreparedStructured(context.Context, peoplesweep.PreparedStructuredRequest) (peoplesweep.StructuredResponse, error) {
	r.calls++
	return peoplesweep.StructuredResponse{}, errors.New("cursor-only sweep must not call provider")
}

func (r *noProviderSweepRunner) RunStructured(context.Context, peoplesweep.StructuredRequest) (peoplesweep.StructuredResponse, error) {
	r.calls++
	return peoplesweep.StructuredResponse{}, errors.New("cursor-only sweep must not call provider")
}

func TestPersonSweepWorkerFilteredNoTextAdvancesCursorAndClearsWork(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": {
			Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1",
			Model: "gpt-test", Auth: peoplesweep.AuthBearer,
			Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
			OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
			RetentionPosture: "zero_retention",
			TrainingPosture:  "no_training", AllowedSources: []peoplesweep.SourceClass{
				peoplesweep.SourceConversationText}, SourceSince: "2027-01-01", RequestTimeout: time.Second,
		}}}
	config.ApplyDefaults()
	profile, err := config.Profile()
	requirements.NoError(err)
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), profile.AllowSensitive)
	requirements.NoError(err)
	key := peoplesweep.CursorKey{PersonID: f.alicePersonID,
		SourceLane:         peoplesweep.SourceConversationText,
		ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint}
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	requirements.NoError(err)
	requirements.Len(cursors, 1)
	checks.Zero(cursors[0].OptimisticSequence)
	f.insertMessage(t, "filtered-no-text", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	highWater := latestPersonSweepSequence(t, f.store)
	requirements.Equal(int64(1), highWater)

	runner := &noProviderSweepRunner{}
	ids := []string{"run-cursor-only", "attempt-cursor-only"}
	worker := peoplesweep.Worker{Config: config, Store: f.store,
		Source:  cursorOnlyAssemblySource{Store: f.store},
		Context: peoplesweep.NewContextRetriever(f.store), Sink: f.store, Runner: runner,
		Catalog: f.store, Clock: func() time.Time {
			return time.Date(2026, 8, 23, 13, 0, 0, 0, time.UTC)
		}, NewID: func() string {
			id := ids[0]
			ids = ids[1:]
			return id
		}, WorkerID: "worker-cursor-only"}
	result, err := worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual,
		Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1})
	requirements.NoError(err)
	checks.Equal(1, result.PeopleAttempted)
	checks.Equal(1, result.PeopleSucceeded)
	checks.Zero(runner.calls)

	var sequence int64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT optimistic_sequence FROM person_sweep_cursors
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?`), f.alicePersonID, key.SourceLane,
		key.ProgramFingerprint, key.CatalogFingerprint).Scan(&sequence))
	checks.Equal(highWater, sequence)
	var workRows int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ?`), f.alicePersonID).Scan(&workRows))
	checks.Zero(workRows)
	var attemptStatus, failureClass string
	var generationID sql.NullInt64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT status, failure_class, generation_id FROM person_sweep_attempts WHERE id = ?`),
		"attempt-cursor-only").Scan(&attemptStatus, &failureClass, &generationID))
	checks.Equal("succeeded", attemptStatus)
	checks.Empty(failureClass)
	requirements.True(generationID.Valid)
	var provider, providerVersion, model, modelVersion string
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT provider, provider_version, model, model_version
		FROM person_fact_generations WHERE id = ?`), generationID.Int64).
		Scan(&provider, &providerVersion, &model, &modelVersion))
	checks.Equal(peoplesweep.StatusOnlyProvider, provider)
	checks.Equal(peoplesweep.StatusOnlyProviderVersion, providerVersion)
	checks.Equal(peoplesweep.StatusOnlyModel, model)
	checks.Equal(peoplesweep.StatusOnlyModelVersion, modelVersion)
	for name, query := range map[string]string{
		"claims":        `SELECT COUNT(*) FROM person_fact_claims WHERE generation_id = ?`,
		"status events": `SELECT COUNT(*) FROM person_fact_evidence_status_events WHERE generation_id = ?`,
		"batches":       `SELECT COUNT(*) FROM person_sweep_batches WHERE attempt_id = 'attempt-cursor-only'`,
		"daily usage":   `SELECT COUNT(*) FROM person_sweep_daily_usage`,
	} {
		t.Run(name, func(t *testing.T) {
			var count int
			args := []any{}
			if name == "claims" || name == "status events" {
				args = append(args, generationID.Int64)
			}
			require.NoError(t, f.store.DB().QueryRowContext(
				t.Context(), f.store.Rebind(query), args...).Scan(&count))
			assert.Zero(t, count)
		})
	}
}
