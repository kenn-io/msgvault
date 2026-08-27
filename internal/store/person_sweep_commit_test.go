package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

type personSweepApplyFixture struct {
	store       *Store
	personID    int64
	runID       string
	attemptID   string
	lease       peoplesweep.Lease
	cursor      peoplesweep.Cursor
	request     peoplesweep.ApplyRequest
	reservation peoplesweep.BudgetReservation
	targets     map[string]personfacts.TargetDescriptor
}

func personSweepApplyBudget() peoplesweep.BudgetConfig {
	return peoplesweep.BudgetConfig{
		MaxRequestsPerPerson: 10, MaxInputTokensPerPerson: 10_000,
		MaxOutputTokensPerPerson: 10_000, MaxRequestsPerRun: 100,
		MaxInputTokensPerRun: 100_000, MaxOutputTokensPerRun: 100_000,
		MaxEstimatedCostMicroUSDPerRun: 100_000, MaxRequestsPerDay: 1_000,
		MaxInputTokensPerDay: 1_000_000, MaxOutputTokensPerDay: 1_000_000,
		MaxEstimatedCostMicroUSDPerDay:     1_000_000,
		InputCostMicroUSDPerMillionTokens:  1_000_000,
		OutputCostMicroUSDPerMillionTokens: 1_000_000,
	}
}

func newPersonSweepApplyFixture(t *testing.T, suffix string, provider bool) personSweepApplyFixture {
	t.Helper()
	st, personID, targets := newPersonFactProjectionStore(t)
	program := strings.Repeat("a", 64)
	profile := strings.Repeat("b", 64)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	key := peoplesweep.CursorKey{PersonID: personID, SourceLane: peoplesweep.SourceConversationText,
		ProgramFingerprint: program, CatalogFingerprint: catalog.Fingerprint}
	cursors, err := st.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	require.NoError(t, err)
	require.Len(t, cursors, 1)
	envelope := []peoplesweep.GenerationCursor{{Key: key,
		Mode: peoplesweep.GenerationCursorOptimistic, CursorFrom: 0, CursorThrough: 1}}
	sources, sourceHash, err := peoplesweep.PersonFactSourceCursors(envelope)
	require.NoError(t, err)
	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	runID, attemptID := "run-"+suffix, "attempt-"+suffix
	_, err = st.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{ID: runID,
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: program, CatalogFingerprint: catalog.Fingerprint,
		ProviderFingerprint: profile, StartedAt: personFactLedgerNow})
	require.NoError(t, err)
	require.NoError(t, st.StartPersonSweepAttempt(t.Context(), peoplesweep.StartAttempt{
		ID: attemptID, RunID: runID, PersonID: personID, LeaseFence: 1,
		Mode: peoplesweep.RunIncremental, CursorEnvelope: envelope,
		EnvelopeHash: hex.EncodeToString(digest[:]), StartedAt: personFactLedgerNow,
	}))
	lease := peoplesweep.Lease{PersonID: personID, WorkerID: "worker-" + suffix, Fence: 1,
		ExpiresAt: time.Now().UTC().Add(time.Hour)}
	_, err = st.db.ExecContext(t.Context(), `UPDATE person_sweep_work SET
		dirty_through_sequence = 1, available_at = CURRENT_TIMESTAMP, attempt_count = 0,
		lease_owner = ?, lease_until = ?, lease_fence = ? WHERE person_id = ?`, lease.WorkerID,
		st.dialect.TimestampParam(lease.ExpiresAt), lease.Fence, personID)
	require.NoError(t, err)
	generation := personFactProjectionInput(personID, suffix, nil, nil)
	generation.SourceCursors = sources
	generation.ProgramID = "sweep-fixture"
	generation.ProgramVersion = "v1"
	generation.ProgramFingerprint = program
	generation.CatalogFingerprint = catalog.Fingerprint
	generation.Policy = personfacts.PolicyContext{ProviderPolicyFingerprint: profile}
	generation.ResolvedAt = personFactLedgerNow
	request := peoplesweep.ApplyRequest{Lease: lease, RunID: runID, AttemptID: attemptID,
		Generation: generation, CursorEnvelope: envelope,
		CursorAdvances: []peoplesweep.CursorAdvance{{Key: key,
			Mode: peoplesweep.GenerationCursorOptimistic, ExpectedSequence: 0,
			NextSequence: 1, EnvelopeHash: sourceHash}}, CompletedAt: personFactLedgerNow.Add(time.Minute)}
	fixture := personSweepApplyFixture{store: st, personID: personID, runID: runID,
		attemptID: attemptID, lease: lease, cursor: cursors[0], request: request, targets: targets}
	_, err = st.db.ExecContext(t.Context(), `INSERT INTO person_inference_profiles
		(fingerprint,provider_kind,endpoint,model,api_key_env,allow_anonymous,
		 retention_posture,training_posture,allowed_sources,source_since,allow_sensitive,policy_json)
		VALUES (?, 'fixture-provider', '', 'fixture-model', '', FALSE, 'none', 'none', '[]', '', FALSE, '{}')`, profile)
	require.NoError(t, err)
	if !provider {
		request.Generation.Provider = peoplesweep.StatusOnlyProvider
		request.Generation.ProviderVersion = peoplesweep.StatusOnlyProviderVersion
		request.Generation.Model = peoplesweep.StatusOnlyModel
		request.Generation.ModelVersion = peoplesweep.StatusOnlyModelVersion
		fixture.request = request
		return fixture
	}
	request.Generation.Provider = "fixture-provider"
	request.Generation.ProviderVersion = "provider-v1"
	request.Generation.Model = "fixture-model"
	request.Generation.ModelVersion = "model-v1"
	request.Generation.Claims = []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, suffix),
	}
	_, err = st.db.ExecContext(t.Context(), `INSERT INTO person_inference_consents
		(profile_fingerprint, granted_by) VALUES (?, 'test')`, profile)
	require.NoError(t, err)
	budget := personSweepApplyBudget()
	reservation, err := st.ReservePersonSweepBudget(t.Context(), peoplesweep.BudgetReservationRequest{
		RunID: runID, AttemptID: attemptID, BatchOrdinal: 0, PersonID: personID,
		ProviderFingerprint: profile, UTCDate: "2026-08-23", InputHash: strings.Repeat("c", 64),
		ItemCount: 1, EstimatedRequests: 1, EstimatedInputTokens: 3,
		EstimatedOutputTokens: 2, EstimatedCostMicroUSD: 5, Budget: budget})
	require.NoError(t, err)
	require.NoError(t, st.MarkPersonSweepBudgetStarted(t.Context(), reservation))
	request.Batches = []peoplesweep.CompletedBatch{{Ordinal: 0, ReservationID: reservation.ID,
		InputHash: reservation.Request.InputHash, ProviderRequestID: "request-1",
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
		Usage: peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1}, ActualCostMicroUSD: 3,
		Latency: time.Second}}
	request.Usage = peoplesweep.Usage{Requests: 1, InputTokens: 3, OutputTokens: 2,
		EstimatedCostMicroUSD: 5}
	fixture.request, fixture.reservation = request, reservation
	return fixture
}

func TestApplyPersonSweepCommitsFactsUsageAndCursorAtomically(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "atomic", true)
	result, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)
	checks.True(result.Mutations.GenerationInserted)
	checks.Equal(1, result.Mutations.ClaimRowsInserted)
	checks.Equal(1, result.Mutations.ProjectionRowsWritten)
	checks.Equal(1, result.Mutations.BatchRowsReconciled)
	checks.Equal(1, result.Mutations.CursorRowsAdvanced)
	var sequence int64
	requirements.NoError(f.store.db.QueryRow(`SELECT optimistic_sequence FROM person_sweep_cursors
		WHERE person_id = ?`, f.personID).Scan(&sequence))
	checks.Equal(int64(1), sequence)
	var batchStatus, attemptStatus string
	requirements.NoError(f.store.db.QueryRow(`SELECT status FROM person_sweep_batches
		WHERE attempt_id = ? AND batch_ordinal = 0`, f.attemptID).Scan(&batchStatus))
	requirements.NoError(f.store.db.QueryRow(`SELECT status FROM person_sweep_attempts
		WHERE id = ?`, f.attemptID).Scan(&attemptStatus))
	checks.Equal("succeeded", batchStatus)
	checks.Equal("succeeded", attemptStatus)
}

func TestApplyPersonSweepRequeuesExplicitlyDeferredCursorWork(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := newPersonSweepApplyFixture(t, "deferred-cursor-work", false)
	f.request.DeferredCursorWork = true

	_, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	must.NoError(err)
	var count int
	var owner string
	must.NoError(f.store.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(lease_owner), '')
		FROM person_sweep_work WHERE person_id = ?`, f.personID).Scan(&count, &owner))
	checks.Equal(1, count)
	checks.Empty(owner)
}

func TestApplyPersonSweepStoresGenerationIDAndKeyOnAttempt(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "identity", true)
	var beforeID sql.NullInt64
	var beforeKey string
	requirements.NoError(f.store.db.QueryRow(`SELECT generation_id, generation_key
		FROM person_sweep_attempts WHERE id = ?`, f.attemptID).Scan(&beforeID, &beforeKey))
	checks.False(beforeID.Valid)
	checks.Empty(beforeKey)
	result, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)
	var generationID int64
	var generationKey string
	requirements.NoError(f.store.db.QueryRow(`SELECT generation_id, generation_key
		FROM person_sweep_attempts WHERE id = ?`, f.attemptID).Scan(&generationID, &generationKey))
	checks.Equal(result.Generation.GenerationID, generationID)
	checks.Equal(result.Generation.GenerationKey, generationKey)
}

func TestApplyPersonSweepRollsBackAtEveryStage(t *testing.T) {
	for _, stage := range []string{
		"locks", "consent", "evidence_status", "claim", "decision", "projection", "usage", "cursor_cas",
	} {
		t.Run(stage, func(t *testing.T) {
			f := newPersonSweepApplyFixture(t, "rollback-"+stage, true)
			before := personSweepApplyRollbackSnapshot(t, f.store)
			ctx := withPersonSweepApplyFailpoint(t.Context(), func(got string) error {
				if got == stage {
					return errors.New("injected " + stage)
				}
				return nil
			})
			_, err := f.store.ApplyPersonSweep(ctx, f.request)
			require.ErrorContains(t, err, "injected "+stage)
			assert.Equal(t, before, personSweepApplyRollbackSnapshot(t, f.store))
		})
	}
}

type personSweepRollbackSnapshot struct {
	FactRows map[string]int64
	Tables   map[string][][]string
}

func personSweepApplyRollbackSnapshot(t *testing.T, st *Store) personSweepRollbackSnapshot {
	t.Helper()
	queries := map[string]string{
		"runs":        `SELECT * FROM person_sweep_runs ORDER BY id`,
		"attempts":    `SELECT * FROM person_sweep_attempts ORDER BY id`,
		"batches":     `SELECT * FROM person_sweep_batches ORDER BY attempt_id, batch_ordinal`,
		"daily_usage": `SELECT * FROM person_sweep_daily_usage ORDER BY utc_day`,
		"work":        `SELECT * FROM person_sweep_work ORDER BY person_id`,
		"cursors":     `SELECT * FROM person_sweep_cursors ORDER BY person_id, source_lane, program_fingerprint, catalog_fingerprint`,
	}
	tables := make(map[string][][]string, len(queries))
	for name, query := range queries {
		rows, err := st.db.Query(query)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for index := range values {
				destinations[index] = &values[index]
			}
			require.NoError(t, rows.Scan(destinations...))
			encoded := make([]string, len(values))
			for index, value := range values {
				switch typed := value.(type) {
				case nil:
					encoded[index] = "<NULL>"
				case []byte:
					encoded[index] = string(typed)
				case time.Time:
					encoded[index] = typed.UTC().Format(time.RFC3339Nano)
				default:
					encoded[index] = fmt.Sprint(typed)
				}
			}
			tables[name] = append(tables[name], encoded)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return personSweepRollbackSnapshot{FactRows: personFactProjectionCounts(t, st), Tables: tables}
}

func TestApplyPersonSweepRejectsReclaimedLease(t *testing.T) {
	f := newPersonSweepApplyFixture(t, "reclaimed", true)
	_, err := f.store.db.Exec(`UPDATE person_sweep_work SET lease_owner = 'new-worker', lease_fence = 2
		WHERE person_id = ?`, f.personID)
	require.NoError(t, err)
	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	require.ErrorIs(t, err, peoplesweep.ErrLeaseLost)
	assert.Zero(t, personFactProjectionRowCount(t, f.store, "person_fact_generations"))
}

func TestApplyPersonSweepRevokedConsentChargesUsageOnly(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "revoked-consent", true)
	_, err := f.store.db.Exec(`UPDATE person_inference_consents SET revoked_by = 'test',
		revoked_at = CURRENT_TIMESTAMP WHERE profile_fingerprint = ?`,
		f.request.Generation.Policy.ProviderPolicyFingerprint)
	requirements.NoError(err)
	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.ErrorIs(err, peoplesweep.ErrPersonSweepConsentRevoked)
	checks.Zero(personFactProjectionRowCount(t, f.store, "person_fact_generations"))
	requirements.NoError(f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{
		Lease: f.lease, AttemptID: f.attemptID, Class: peoplesweep.FailurePolicy,
		RetryAt: personFactLedgerNow.Add(time.Hour), Reservations: []peoplesweep.BudgetReservation{f.reservation},
		Completed: []peoplesweep.CompletedUsage{{BatchOrdinal: 0, ProviderRequestID: "request-1",
			Usage: peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1}, Latency: time.Second}},
		FinalizedAt: personFactLedgerNow.Add(time.Minute),
	}))
	var status string
	var actualRequests int
	requirements.NoError(f.store.db.QueryRow(`SELECT status, actual_requests FROM person_sweep_batches
		WHERE attempt_id = ?`, f.attemptID).Scan(&status, &actualRequests))
	checks.Equal("succeeded", status)
	checks.Equal(1, actualRequests)
	checks.Zero(personFactProjectionRowCount(t, f.store, "person_fact_generations"))
	var generationID sql.NullInt64
	var generationKey string
	requirements.NoError(f.store.db.QueryRow(`SELECT generation_id, generation_key
		FROM person_sweep_attempts WHERE id = ?`, f.attemptID).Scan(&generationID, &generationKey))
	checks.False(generationID.Valid)
	checks.Empty(generationKey)
}

func TestApplyPersonSweepRejectsUnboundCursorAdvance(t *testing.T) {
	tests := map[string]func(*peoplesweep.ApplyRequest){
		"missing": func(r *peoplesweep.ApplyRequest) { r.CursorAdvances = nil },
		"extra":   func(r *peoplesweep.ApplyRequest) { r.CursorAdvances = append(r.CursorAdvances, r.CursorAdvances[0]) },
		"hash":    func(r *peoplesweep.ApplyRequest) { r.CursorAdvances[0].EnvelopeHash = strings.Repeat("d", 64) },
		"range":   func(r *peoplesweep.ApplyRequest) { r.CursorAdvances[0].NextSequence++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newPersonSweepApplyFixture(t, "unbound-"+name, false)
			mutate(&f.request)
			_, err := f.store.ApplyPersonSweep(t.Context(), f.request)
			require.Error(t, err)
			assert.Zero(t, personFactProjectionRowCount(t, f.store, "person_fact_generations"))
		})
	}
}

func TestApplyPersonSweepReturnsSortedResolutions(t *testing.T) {
	f := newPersonSweepApplyFixture(t, "sorted", true)
	f.request.Generation.Claims = []personfacts.ProposedClaim{
		personFactProjectionClaim(f.personID, f.targets[AttributeSlugAskMeAbout], `["systems"]`, "sorted-z"),
		personFactProjectionClaim(f.personID, f.targets[AttributeSlugPrimaryChannel], `"chat"`, "sorted-a"),
	}
	result, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	require.NoError(t, err)
	require.Len(t, result.Generation.Resolutions, 2)
	for i := 1; i < len(result.Generation.Resolutions); i++ {
		left, right := result.Generation.Resolutions[i-1].Target, result.Generation.Resolutions[i].Target
		assert.LessOrEqual(t, string(left.Kind)+"\x00"+left.Key, string(right.Kind)+"\x00"+right.Key)
	}
}

func TestApplyPersonSweepReplayReturnsFullGenerationAndZeroFactMutations(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "replay-first", true)
	first, err := f.store.ApplyPersonFactGenerationContext(t.Context(), f.request.Generation, nil)
	requirements.NoError(err)
	replay, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)
	checks.Equal(*first, replay.Generation)
	checks.False(replay.Mutations.GenerationInserted)
	checks.Zero(replay.Mutations.ClaimRowsInserted)
	checks.Zero(replay.Mutations.EvidenceStatusRowsInserted)
	checks.Zero(replay.Mutations.ResolutionRowsInserted)
	checks.Zero(replay.Mutations.DecisionRowsInserted)
	checks.Zero(replay.Mutations.ProjectionRowsWritten)
	checks.False(replay.Mutations.VCardRevisionBumped)
	checks.Equal(1, replay.Mutations.BatchRowsReconciled)
	checks.Equal(1, replay.Mutations.CursorRowsAdvanced)
	checks.Equal(1, replay.Mutations.AttemptRowsSucceeded)
}

func TestPersonSweepStatusOnlyGenerationUsesHostIdentity(t *testing.T) {
	f := newPersonSweepApplyFixture(t, "status-host", false)
	result, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	require.NoError(t, err)
	assert.NotEmpty(t, result.Generation.GenerationKey)
	base, err := personfacts.PreparePersonFactGeneration(t.Context(), f.request.Generation, nil)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*personfacts.GenerationInput){
		"provider":         func(v *personfacts.GenerationInput) { v.Provider += "-changed" },
		"provider version": func(v *personfacts.GenerationInput) { v.ProviderVersion += "-changed" },
		"model":            func(v *personfacts.GenerationInput) { v.Model += "-changed" },
		"model version":    func(v *personfacts.GenerationInput) { v.ModelVersion += "-changed" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := f.request.Generation
			mutate(&changed)
			prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), changed, nil)
			require.NoError(t, err)
			assert.NotEqual(t, base.GenerationKey(), prepared.GenerationKey())
		})
	}
}

func TestPersonSweepStatusOnlyGenerationRejectsSpoofedIdentity(t *testing.T) {
	for _, field := range []string{"provider", "provider-version", "model", "model-version", "claim", "usage", "batch"} {
		t.Run(field, func(t *testing.T) {
			f := newPersonSweepApplyFixture(t, "status-spoof-"+field, false)
			switch field {
			case "provider":
				f.request.Generation.Provider = "spoof"
			case "provider-version":
				f.request.Generation.ProviderVersion = "spoof"
			case "model":
				f.request.Generation.Model = "spoof"
			case "model-version":
				f.request.Generation.ModelVersion = "spoof"
			case "claim":
				f.request.Generation.Claims = []personfacts.ProposedClaim{{}}
			case "usage":
				f.request.Usage.Requests = 1
			case "batch":
				f.request.Batches = []peoplesweep.CompletedBatch{{Ordinal: 0}}
			}
			_, err := f.store.ApplyPersonSweep(t.Context(), f.request)
			require.Error(t, err)
		})
	}
}

func TestPersonSweepStatusOnlyGenerationBindsDurableProfilePolicy(t *testing.T) {
	t.Run("missing profile", func(t *testing.T) {
		f := newPersonSweepApplyFixture(t, "status-profile-missing", false)
		_, err := f.store.db.ExecContext(t.Context(), `DELETE FROM person_inference_profiles
			WHERE fingerprint = ?`, f.request.Generation.Policy.ProviderPolicyFingerprint)
		require.NoError(t, err)
		_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
		require.ErrorContains(t, err, "profile")
		assert.Zero(t, personFactProjectionRowCount(t, f.store, "person_fact_generations"))
	})
	t.Run("forged policy", func(t *testing.T) {
		f := newPersonSweepApplyFixture(t, "status-policy-forged", false)
		f.request.Generation.Policy.AllowSensitive = true
		_, err := f.store.ApplyPersonSweep(t.Context(), f.request)
		require.ErrorContains(t, err, "profile")
		assert.Zero(t, personFactProjectionRowCount(t, f.store, "person_fact_generations"))
	})
}

func TestPersonSweepBackstopAppliesWithoutOptimisticAdvance(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "backstop", false)
	f.request.CursorEnvelope[0].Mode = peoplesweep.GenerationCursorBackstop
	f.request.CursorEnvelope[0].CursorThrough = 0
	f.request.CursorEnvelope[0].ReconcileToKey = "0001"
	f.request.CursorEnvelope[0].BackstopUpperKey = "0001"
	sources, hash, err := peoplesweep.PersonFactSourceCursors(f.request.CursorEnvelope)
	requirements.NoError(err)
	f.request.Generation.SourceCursors = sources
	f.request.CursorAdvances = []peoplesweep.CursorAdvance{{Key: f.request.CursorEnvelope[0].Key,
		Mode: peoplesweep.GenerationCursorBackstop, EnvelopeHash: hash,
		NextReconcileKey: "0001", CapturedBackstopUpperKey: "0001", BackstopComplete: true}}
	encoded, err := json.Marshal(f.request.CursorEnvelope)
	requirements.NoError(err)
	digest := sha256.Sum256(encoded)
	_, err = f.store.db.Exec(`UPDATE person_sweep_attempts SET cursor_envelope_json = ?, envelope_hash = ?
		WHERE id = ?`, string(encoded), hex.EncodeToString(digest[:]), f.attemptID)
	requirements.NoError(err)
	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)
	var optimistic int64
	var backstop any
	var upper, after string
	requirements.NoError(f.store.db.QueryRow(`SELECT optimistic_sequence, backstop_upper_key,
		backstop_after_key, last_backstop_at FROM person_sweep_cursors WHERE person_id = ?`,
		f.personID).Scan(&optimistic, &upper, &after, &backstop))
	checks.Zero(optimistic)
	checks.Empty(upper)
	checks.Empty(after)
	checks.NotNil(backstop)
}

func TestPersonSweepPartialBackstopPersistsRangeWithoutCompletionStamp(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "partial-backstop", false)
	f.request.CursorEnvelope[0] = peoplesweep.GenerationCursor{
		Key: f.request.CursorEnvelope[0].Key, Mode: peoplesweep.GenerationCursorBackstop,
		ReconcileToKey: "0001", BackstopUpperKey: "0002",
	}
	sources, hash, err := peoplesweep.PersonFactSourceCursors(f.request.CursorEnvelope)
	requirements.NoError(err)
	f.request.Generation.SourceCursors = sources
	f.request.CursorAdvances = []peoplesweep.CursorAdvance{{
		Key: f.request.CursorEnvelope[0].Key, Mode: peoplesweep.GenerationCursorBackstop,
		NextReconcileKey: "0001", CapturedBackstopUpperKey: "0002", EnvelopeHash: hash,
	}}
	encoded, err := json.Marshal(f.request.CursorEnvelope)
	requirements.NoError(err)
	digest := sha256.Sum256(encoded)
	_, err = f.store.db.Exec(`UPDATE person_sweep_attempts SET cursor_envelope_json = ?, envelope_hash = ?
		WHERE id = ?`, string(encoded), hex.EncodeToString(digest[:]), f.attemptID)
	requirements.NoError(err)

	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)
	var upper, after string
	var completed any
	requirements.NoError(f.store.db.QueryRow(`SELECT backstop_upper_key, backstop_after_key,
		last_backstop_at FROM person_sweep_cursors WHERE person_id = ?`, f.personID).Scan(
		&upper, &after, &completed))
	checks.Equal("0002", upper)
	checks.Equal("0001", after)
	checks.Nil(completed)
	var workRows int
	requirements.NoError(f.store.db.QueryRow(`SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ?`,
		f.personID).Scan(&workRows))
	checks.Equal(1, workRows)
}

func TestApplyPersonSweepAdvancesPluralModesInCASOrder(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "plural-cas", false)
	key := f.request.CursorEnvelope[0].Key
	_, err := f.store.db.Exec(`UPDATE person_sweep_cursors SET reconcile_upper_key = '0001',
		reconcile_after_key = '', reconciliation_complete = FALSE WHERE person_id = ?`, f.personID)
	requirements.NoError(err)
	f.request.CursorEnvelope = []peoplesweep.GenerationCursor{
		{Key: key, Mode: peoplesweep.GenerationCursorOptimistic, CursorThrough: 1},
		{Key: key, Mode: peoplesweep.GenerationCursorReconciliation, ReconcileToKey: "0001"},
		{Key: key, Mode: peoplesweep.GenerationCursorBackstop, ReconcileToKey: "0002", BackstopUpperKey: "0002"},
	}
	sources, hash, err := peoplesweep.PersonFactSourceCursors(f.request.CursorEnvelope)
	requirements.NoError(err)
	f.request.Generation.SourceCursors = sources
	f.request.CursorAdvances = []peoplesweep.CursorAdvance{
		{Key: key, Mode: peoplesweep.GenerationCursorOptimistic, NextSequence: 1, EnvelopeHash: hash},
		{Key: key, Mode: peoplesweep.GenerationCursorReconciliation, NextReconcileKey: "0001",
			ReconciliationDone: true, EnvelopeHash: hash},
		{Key: key, Mode: peoplesweep.GenerationCursorBackstop, NextReconcileKey: "0002",
			CapturedBackstopUpperKey: "0002", BackstopComplete: true, EnvelopeHash: hash},
	}
	encoded, err := json.Marshal(f.request.CursorEnvelope)
	requirements.NoError(err)
	digest := sha256.Sum256(encoded)
	_, err = f.store.db.Exec(`UPDATE person_sweep_attempts SET cursor_envelope_json = ?, envelope_hash = ?
		WHERE id = ?`, string(encoded), hex.EncodeToString(digest[:]), f.attemptID)
	requirements.NoError(err)
	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)
}

func TestApplyPersonSweepRecomputesGenerationKey(t *testing.T) {
	f := newPersonSweepApplyFixture(t, "generation-key", false)
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), f.request.Generation, nil)
	require.NoError(t, err)
	mutations := map[string]func(*personfacts.GenerationInput){
		"program id":          func(v *personfacts.GenerationInput) { v.ProgramID += "-changed" },
		"program version":     func(v *personfacts.GenerationInput) { v.ProgramVersion = "v2" },
		"program fingerprint": func(v *personfacts.GenerationInput) { v.ProgramFingerprint = strings.Repeat("f", 64) },
		"catalog":             func(v *personfacts.GenerationInput) { v.CatalogFingerprint += "-changed" },
		"provider":            func(v *personfacts.GenerationInput) { v.Provider += "-changed" },
		"provider version":    func(v *personfacts.GenerationInput) { v.ProviderVersion += "-changed" },
		"model":               func(v *personfacts.GenerationInput) { v.Model += "-changed" },
		"model version":       func(v *personfacts.GenerationInput) { v.ModelVersion += "-changed" },
		"policy":              func(v *personfacts.GenerationInput) { v.Policy.AllowSensitive = !v.Policy.AllowSensitive },
		"cursor":              func(v *personfacts.GenerationInput) { v.SourceCursors[0].End += "-changed" },
		"claim": func(v *personfacts.GenerationInput) {
			v.Claims = []personfacts.ProposedClaim{personFactProjectionClaim(
				f.personID, f.targets[AttributeSlugPrimaryChannel], `"email"`, "changed-claim")}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := f.request.Generation
			changed.SourceCursors = append([]personfacts.SourceCursor(nil), changed.SourceCursors...)
			mutate(&changed)
			changedPrepared, err := personfacts.PreparePersonFactGeneration(t.Context(), changed, nil)
			require.NoError(t, err)
			assert.NotEqual(t, prepared.GenerationKey(), changedPrepared.GenerationKey())
		})
	}
	resolvedOnly := f.request.Generation
	resolvedOnly.ResolvedAt = resolvedOnly.ResolvedAt.Add(time.Hour)
	resolvedPrepared, err := personfacts.PreparePersonFactGeneration(t.Context(), resolvedOnly, nil)
	require.NoError(t, err)
	assert.Equal(t, prepared.GenerationKey(), resolvedPrepared.GenerationKey())
}

func TestApplyPersonSweepStoresClaimMutationInGenerationKey(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "stored-claim-key", true)
	base, err := personfacts.PreparePersonFactGeneration(t.Context(), f.request.Generation, nil)
	requirements.NoError(err)
	f.request.Generation.Claims[0].SubmittedValue = json.RawMessage(`"email"`)
	changed, err := personfacts.PreparePersonFactGeneration(t.Context(), f.request.Generation, nil)
	requirements.NoError(err)
	checks.NotEqual(base.GenerationKey(), changed.GenerationKey())
	result, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err)
	var stored string
	requirements.NoError(f.store.db.QueryRow(`SELECT generation_key FROM person_sweep_attempts
		WHERE id = ?`, f.attemptID).Scan(&stored))
	checks.Equal(changed.GenerationKey(), result.Generation.GenerationKey)
	checks.Equal(changed.GenerationKey(), stored)
}

func TestApplyPersonSweepGenerationCollisionRollsBack(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "collision", true)
	_, err := f.store.ApplyPersonFactGenerationContext(t.Context(), f.request.Generation, nil)
	requirements.NoError(err)
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), f.request.Generation, nil)
	requirements.NoError(err)
	_, err = f.store.db.ExecContext(t.Context(), `UPDATE person_fact_generations
		SET provider_version = 'different-bytes' WHERE generation_key = ?`, prepared.GenerationKey())
	requirements.NoError(err)
	_, err = f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.ErrorIs(err, ErrPersonFactKeyCollision)
	var sequence int64
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), `SELECT optimistic_sequence
		FROM person_sweep_cursors WHERE person_id = ?`, f.personID).Scan(&sequence))
	checks.Zero(sequence)
	var status string
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), `SELECT status
		FROM person_sweep_attempts WHERE id = ?`, f.attemptID).Scan(&status))
	checks.Equal("running", status)
	var batchStatus string
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), `SELECT status
		FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = 0`,
		f.attemptID).Scan(&batchStatus))
	checks.Equal("running", batchStatus)
	var generationID sql.NullInt64
	var generationKey string
	requirements.NoError(f.store.db.QueryRowContext(t.Context(), `SELECT generation_id, generation_key
		FROM person_sweep_attempts WHERE id = ?`, f.attemptID).Scan(&generationID, &generationKey))
	checks.False(generationID.Valid)
	checks.Empty(generationKey)
}

func TestPersonSweepApplyLockPlanOrdersDailyUsageBeforeWork(t *testing.T) {
	assert.Equal(t, []personSweepLockCoordinate{
		{kind: personSweepLockDailyUsage, value: "2026-08-22"},
		{kind: personSweepLockDailyUsage, value: "2026-08-23"},
		{kind: personSweepLockBatch, value: "2026-08-23", ordinal: 1},
		{kind: personSweepLockBatch, value: "2026-08-22", ordinal: 2},
		{kind: personSweepLockBatch, value: "2026-08-23", ordinal: 3},
		{kind: personSweepLockWork, personID: 7},
	}, personSweepUsageWorkLockPlan([]personSweepBatchCoordinate{
		{ordinal: 3, day: "2026-08-23"}, {ordinal: 2, day: "2026-08-22"},
		{ordinal: 1, day: "2026-08-23"},
	}, 7))
}

type blockingSweepAligner struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (a blockingSweepAligner) Align(context.Context, personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
	a.entered <- struct{}{}
	<-a.release
	return personfacts.AlignmentResult{Accepted: true, SourceVersion: "source-v1",
		ContentSHA256: strings.Repeat("e", 64)}, nil
}

func TestApplyPersonSweepPreparesBeforeOuterTransaction(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "prepare-before-tx", true)
	start, end := int64(0), int64(4)
	claim := f.request.Generation.Claims[0]
	claim.Evidence[0].SourceClass = personfacts.EvidenceArchive
	claim.Evidence[0].SourceURL = ""
	claim.Evidence[0].SourceRef = "fixture-source"
	claim.Evidence[0].SpanStart, claim.Evidence[0].SpanEnd = &start, &end
	claim.Evidence[0].ContentSHA256 = strings.Repeat("e", 64)
	claim.Evidence[0].SourceVersion = "source-v1"
	f.request.Generation.Claims = []personfacts.ProposedClaim{claim}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.store.applyPersonSweepWithAligner(t.Context(), f.request,
			blockingSweepAligner{entered: entered, release: release})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		requirements.FailNow("aligner did not block")
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := f.store.db.ExecContext(t.Context(), `UPDATE persons SET display_name = display_name WHERE id = ?`, f.personID)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		requirements.NoError(err)
	case <-time.After(2 * time.Second):
		requirements.FailNow("unrelated write was blocked while alignment was running")
	}
	close(release)
	requirements.NoError(<-done)
}
