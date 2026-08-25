package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

var personFactLedgerNow = time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

func TestPersonFactGenerationEnvelopeOwnsMultipleClaims(t *testing.T) {
	st, personID := newPersonFactLedgerStore(t)
	prepared := preparePersonFactLedgerGeneration(t, personID, "multi", []personfacts.ProposedClaim{
		personFactLedgerClaim(personID, "favorite-color", `"blue"`, "color"),
		personFactLedgerClaim(personID, "favorite-food", `"ramen"`, "food"),
	}, nil)

	stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	assert.Len(t, stored.Claims, 2)
	for _, claim := range stored.Claims {
		assert.Equal(t, stored.Generation.ID, claim.GenerationID)
		assert.Equal(t, stored.Generation.ID, claim.Generation.ID)
	}
}

func TestPersonFactGenerationPersistsProgramFingerprint(t *testing.T) {
	st, personID := newPersonFactLedgerStore(t)
	prepared := preparePersonFactLedgerGeneration(t, personID, "fingerprint", nil, nil)

	stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	assert.Equal(t, strings.Repeat("a", 64), stored.Generation.ProgramFingerprint)
}

func TestPersonFactGenerationPortableTimestampReplayIsExact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	claim := personFactLedgerClaim(personID, "portable-time", `"value"`, "portable-time")
	claim.Evidence[0].EventTime = time.Date(2025, time.March, 1, 2, 3, 4, 123456789, time.UTC)
	claim.Evidence[0].RecordedTime = time.Date(2025, time.March, 2, 3, 4, 5, 234567891, time.UTC)
	validFrom := time.Date(2024, time.March, 1, 0, 0, 0, 345678912, time.UTC)
	validUntil := time.Date(2026, time.March, 1, 0, 0, 0, 456789123, time.UTC)
	claim.ValidFrom, claim.ValidUntil = &validFrom, &validUntil
	input := personFactLedgerGenerationInput(personID, "portable-time",
		[]personfacts.ProposedClaim{claim}, nil)
	input.ResolvedAt = time.Date(2025, time.March, 3, 4, 5, 6, 567891234, time.UTC)
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), input, nil)
	require.NoError(err)
	first := persistPersonFactLedgerGeneration(t, st, prepared, nil)

	replayInput := prepared.Input()
	replayInput.ResolvedAt = replayInput.ResolvedAt.Add(10 * time.Nanosecond)
	replayInput.Claims[0].Evidence[0].EventTime = replayInput.Claims[0].Evidence[0].EventTime.Add(10 * time.Nanosecond)
	replayInput.Claims[0].Evidence[0].RecordedTime = replayInput.Claims[0].Evidence[0].RecordedTime.Add(10 * time.Nanosecond)
	*replayInput.Claims[0].ValidFrom = replayInput.Claims[0].ValidFrom.Add(10 * time.Nanosecond)
	*replayInput.Claims[0].ValidUntil = replayInput.Claims[0].ValidUntil.Add(10 * time.Nanosecond)
	replayed, err := personfacts.PreparePersonFactGeneration(t.Context(), replayInput, nil)
	require.NoError(err)
	assert.Equal(prepared.GenerationKey(), replayed.GenerationKey())
	second := persistPersonFactLedgerGeneration(t, st, replayed, nil)

	assert.Equal(first, second)
	assert.Equal(input.ResolvedAt.Truncate(time.Microsecond), second.Generation.ResolvedAt)
	require.Len(second.Claims, 1)
	assert.Equal(validFrom.Truncate(time.Microsecond), *second.Claims[0].ValidFrom)
	assert.Equal(validUntil.Truncate(time.Microsecond), *second.Claims[0].ValidUntil)
	require.Len(second.Evidence, 1)
	assert.Equal(claim.Evidence[0].EventTime.Truncate(time.Microsecond), second.Evidence[0].Input.EventTime)
	assert.Equal(claim.Evidence[0].RecordedTime.Truncate(time.Microsecond), second.Evidence[0].Input.RecordedTime)
}

func TestPersonFactLedgerStoresInvalidSubmittedValueWithNullNormalizedValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	claim := personFactLedgerClaim(personID, "favorite-number", `{"broken"`, "invalid")
	claim.Target.ValueType = personfacts.ValueInteger
	prepared := preparePersonFactLedgerGeneration(t, personID, "invalid", []personfacts.ProposedClaim{claim}, nil)

	stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	require.Len(stored.Claims, 1)
	assert.Equal(`{"broken"`, string(stored.Claims[0].SubmittedValue))
	assert.Nil(stored.Claims[0].Normalized)

	var normalized any
	require.NoError(st.db.QueryRowContext(t.Context(),
		`SELECT normalized_value_json FROM person_fact_claims WHERE id = ?`,
		stored.Claims[0].ID).Scan(&normalized))
	assert.Nil(normalized)
}

func TestPersonFactLedgerRejectsDuplicateEnvelopeBeforePersistence(t *testing.T) {
	st, personID := newPersonFactLedgerStore(t)
	baseClaim := personFactLedgerClaim(personID, "duplicate", `"value"`, "duplicate")
	duplicateEvidenceClaim := baseClaim
	duplicateEvidenceClaim.Evidence = append(duplicateEvidenceClaim.Evidence,
		duplicateEvidenceClaim.Evidence[0])

	for _, test := range []struct {
		name   string
		claims []personfacts.ProposedClaim
	}{
		{name: "evidence key", claims: []personfacts.ProposedClaim{duplicateEvidenceClaim}},
		{name: "canonical claim", claims: []personfacts.ProposedClaim{baseClaim, baseClaim}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := personfacts.PreparePersonFactGeneration(t.Context(),
				personFactLedgerGenerationInput(personID, "duplicate-"+test.name, test.claims, nil), nil)
			require.Error(t, err)
		})
	}

	for _, table := range []string{
		"person_fact_generations", "person_fact_claims", "person_fact_evidence",
	} {
		var count int
		require.NoError(t, st.db.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM `+table+` WHERE person_id = ?`, personID).Scan(&count))
		assert.Zero(t, count, table)
	}
}

func TestPersonFactLedgerRejectsCanonicalKeyCollision(t *testing.T) {
	t.Run("generation", func(t *testing.T) {
		st, personID := newPersonFactLedgerStore(t)
		prepared := preparePersonFactLedgerGeneration(t, personID, "generation-collision",
			[]personfacts.ProposedClaim{personFactLedgerClaim(personID, "favorite-color", `"blue"`, "generation")}, nil)
		stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
		_, err := st.db.ExecContext(t.Context(),
			`UPDATE person_fact_generations SET program_version = 'tampered' WHERE id = ?`,
			stored.Generation.ID)
		require.NoError(t, err)
		err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
			_, _, insertErr := st.insertPersonFactGenerationTx(t.Context(), tx, prepared)
			return insertErr
		})
		assert.ErrorIs(t, err, ErrPersonFactKeyCollision)
	})

	for _, test := range []struct {
		name   string
		column string
		value  string
	}{
		{name: "evidence", column: "excerpt", value: "tampered"},
		{name: "claim", column: "origin", value: "system"},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, personID := newPersonFactLedgerStore(t)
			prepared := preparePersonFactLedgerGeneration(t, personID, test.name+"-collision",
				[]personfacts.ProposedClaim{personFactLedgerClaim(personID, "favorite-color", `"blue"`, test.name)}, nil)
			stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
			table := "person_fact_claims"
			id := stored.Claims[0].ID
			if test.name == "evidence" {
				table = "person_fact_evidence"
				id = stored.Claims[0].EvidenceIDs[0]
			}
			_, err := st.db.ExecContext(t.Context(),
				`UPDATE `+table+` SET `+test.column+` = ? WHERE id = ?`, test.value, id)
			require.NoError(t, err)
			err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
				generation, _, insertErr := st.insertPersonFactGenerationTx(t.Context(), tx, prepared)
				if insertErr != nil {
					return insertErr
				}
				_, insertErr = st.insertPersonFactClaimsTx(t.Context(), tx, generation, prepared.Claims())
				return insertErr
			})
			assert.ErrorIs(t, err, ErrPersonFactKeyCollision)
		})
	}

	for _, test := range []struct {
		name   string
		table  string
		column string
		value  string
	}{
		{name: "resolution", table: "person_fact_resolutions", column: "provider_policy_fingerprint", value: "tampered"},
		{name: "decision", table: "person_fact_decisions", column: "reason", value: string(personfacts.ReasonInsufficientMargin)},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, personID := newPersonFactLedgerStore(t)
			prepared := preparePersonFactLedgerGeneration(t, personID, test.name+"-collision",
				[]personfacts.ProposedClaim{personFactLedgerClaim(personID, "favorite-color", `"blue"`, test.name)}, nil)
			claimKey, err := personfacts.ClaimKey(prepared.GenerationKey(), prepared.Claims()[0])
			require.NoError(t, err)
			resolution := personfacts.Resolution{
				Target:          personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "favorite-color", Revision: "revision-1"},
				ResolverVersion: personfacts.ResolverVersionV1, InputFingerprint: strings.Repeat("d", 64),
				ResolvedAt: personFactLedgerNow,
				Decisions: []personfacts.Decision{{
					ClaimKey: claimKey, Action: personfacts.DecisionRetained,
					Reason: personfacts.ReasonBelowThreshold,
				}},
			}
			stored := persistPersonFactLedgerGeneration(t, st, prepared, []personfacts.Resolution{resolution})
			id := stored.Resolutions[0].ID
			if test.name == "decision" {
				id = stored.Resolutions[0].Decisions[0].ID
			}
			_, err = st.db.ExecContext(t.Context(),
				`UPDATE `+test.table+` SET `+test.column+` = ? WHERE id = ?`, test.value, id)
			require.NoError(t, err)
			err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
				generation, _, insertErr := st.insertPersonFactGenerationTx(t.Context(), tx, prepared)
				if insertErr != nil {
					return insertErr
				}
				_, insertErr = st.insertPersonFactResolutionTx(t.Context(), tx, generation,
					resolution, prepared.Input().Policy.ProviderPolicyFingerprint)
				return insertErr
			})
			assert.ErrorIs(t, err, ErrPersonFactKeyCollision)
		})
	}
}

func TestPersonFactLedgerReplayHydratesWholeGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	prepared := preparePersonFactLedgerGeneration(t, personID, "replay", []personfacts.ProposedClaim{
		personFactLedgerClaim(personID, "favorite-color", `"blue"`, "first"),
		personFactLedgerClaim(personID, "favorite-food", `"ramen"`, "second"),
	}, nil)
	first := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	require.Len(first.Evidence, 2)
	statusEvidenceKey := prepared.Claims()[0].EvidenceKeys[0]
	persistPersonFactLedgerGeneration(t, st,
		preparePersonFactLedgerGeneration(t, personID, "replay-status", nil,
			[]personfacts.EvidenceStatusChange{{
				EvidenceKey: statusEvidenceKey, SourceVersion: "source-v1", Supported: false,
				Reason: personfacts.EvidenceStatusSourceEdited,
			}}), nil)
	replayInput := prepared.Input()
	replayInput.ResolvedAt = replayInput.ResolvedAt.Add(24 * time.Hour)
	replayed, err := personfacts.PreparePersonFactGeneration(t.Context(), replayInput, nil)
	require.NoError(err)
	require.Equal(prepared.GenerationKey(), replayed.GenerationKey())
	second := persistPersonFactLedgerGeneration(t, st, replayed, nil)

	assert.Equal(first.Generation, second.Generation)
	assert.Equal(first.Claims, second.Claims)
	assert.Equal(first.EvidenceStatusEvents, second.EvidenceStatusEvents)
	assert.Equal(first.Resolutions, second.Resolutions)
	assert.Equal(personFactLedgerNow, second.Generation.ResolvedAt,
		"resolved_at is owned by the first successful writer")
	assert.Len(second.Claims, 2)
	assert.NotNil(second.EvidenceStatusEvents)
	require.Len(second.Evidence, 2)
	for _, evidence := range second.Evidence {
		if evidence.Key != statusEvidenceKey {
			continue
		}
		assert.False(evidence.Supported)
		require.NotNil(evidence.LatestStatus)
		assert.Equal(personfacts.EvidenceStatusSourceEdited, evidence.LatestStatus.Reason)
		return
	}
	assert.Fail("generation replay did not hydrate the status-bearing evidence")
}

func TestPersonFactLedgerReplayHydratesDurableResolutionResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	prepared := preparePersonFactLedgerGeneration(t, personID, "resolution", []personfacts.ProposedClaim{
		personFactLedgerClaim(personID, "favorite-color", `"blue"`, "resolution-color"),
		personFactLedgerClaim(personID, "favorite-food", `"ramen"`, "resolution-food"),
	}, nil)
	colorClaimKey, err := personfacts.ClaimKey(prepared.GenerationKey(), prepared.Claims()[0])
	require.NoError(err)
	foodClaimKey, err := personfacts.ClaimKey(prepared.GenerationKey(), prepared.Claims()[1])
	require.NoError(err)
	projectionLast := personfacts.ProjectionRef{Kind: "person_employment", RowID: 42}
	projectionFirst := personfacts.ProjectionRef{Kind: "person_attribute", RowID: 84}
	transientProjection := personfacts.ProjectionRef{Kind: "transient-plan-only", RowID: 126}
	resolvedAt := time.Date(2025, time.February, 3, 4, 5, 6, 123456789, time.UTC)
	resolution := personfacts.Resolution{
		Target:           personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "favorite-color", Revision: "revision-1"},
		ResolverVersion:  personfacts.ResolverVersionV1,
		InputFingerprint: strings.Repeat("b", 64),
		ResolvedAt:       resolvedAt,
		Decisions: []personfacts.Decision{
			{ClaimKey: colorClaimKey, Action: personfacts.DecisionApplied,
				Reason: personfacts.ReasonAppliedProjection,
				Score:  personfacts.ScoreBreakdown{Total: 900}, Projection: &projectionLast},
			{ClaimKey: foodClaimKey, Action: personfacts.DecisionRetained,
				Reason: personfacts.ReasonPinRetained,
				Score:  personfacts.ScoreBreakdown{Total: 850}, Projection: &projectionFirst},
			{ClaimKey: colorClaimKey, Action: personfacts.DecisionRetained,
				Reason: personfacts.ReasonBelowThreshold,
				Score:  personfacts.ScoreBreakdown{Total: 700}, Projection: &projectionFirst},
		},
		Projections: []personfacts.ProjectionPlan{{
			Operation: personfacts.ProjectionSet, Target: personfacts.TargetRef{
				Kind: personfacts.TargetAttribute, Key: "transient", Revision: "revision-1",
			},
			ClaimKey: colorClaimKey, CurrentRef: &transientProjection, ActiveFrom: resolvedAt,
		}},
	}

	first := persistPersonFactLedgerGeneration(t, st, prepared, []personfacts.Resolution{resolution})
	second := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	require.Len(first.Resolutions, 1)
	assert.Equal(first, second)
	assert.Equal(resolvedAt.Truncate(time.Microsecond), second.Resolutions[0].ResolvedAt)
	assert.Equal([]personfacts.ProjectionRef{projectionFirst, projectionLast},
		second.Resolutions[0].Projections)
	assert.NotContains(second.Resolutions[0].Projections, transientProjection)
	assert.Len(second.Resolutions[0].Decisions, 3)

	replayResolution := resolution
	replayResolution.ResolvedAt = replayResolution.ResolvedAt.Add(10 * time.Nanosecond)
	var replayed personfacts.ResolutionResult
	err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
		var insertErr error
		replayed, insertErr = st.insertPersonFactResolutionTx(t.Context(), tx, first.Generation,
			replayResolution, prepared.Input().Policy.ProviderPolicyFingerprint)
		return insertErr
	})
	require.NoError(err)
	assert.Equal(second.Resolutions[0], replayed)
}

func TestPersonFactEvidenceStatusFalseLatestWins(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	evidenceKey := seedPersonFactEvidence(t, st, personID, "delete")
	statuses := []personfacts.EvidenceStatusChange{
		{EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted},
		{EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: true,
			Reason: personfacts.EvidenceStatusSourceReimported},
		{EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceEdited},
	}
	for index, status := range statuses {
		persistPersonFactLedgerGeneration(t, st,
			preparePersonFactLedgerGeneration(t, personID, "delete-status-"+string(rune('a'+index)), nil,
				[]personfacts.EvidenceStatusChange{status}), nil)
	}

	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID, personfacts.EvidenceFilter{})
	require.NoError(err)
	require.Len(evidence, 1)
	assert.False(evidence[0].Supported)
	require.NotNil(evidence[0].LatestStatus)
	assert.Equal(personfacts.EvidenceStatusSourceEdited, evidence[0].LatestStatus.Reason)
}

func TestPersonFactEvidenceStatusTrueReactivates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	evidenceKey := seedPersonFactEvidence(t, st, personID, "reactivate")
	statuses := []struct {
		suffix    string
		supported bool
		reason    personfacts.EvidenceStatusReason
	}{
		{suffix: "off", supported: false, reason: personfacts.EvidenceStatusScopeUnlinked},
		{suffix: "on", supported: true, reason: personfacts.EvidenceStatusScopeRelinked},
	}
	for _, status := range statuses {
		persistPersonFactLedgerGeneration(t, st,
			preparePersonFactLedgerGeneration(t, personID, "reactivate-"+status.suffix, nil,
				[]personfacts.EvidenceStatusChange{{
					EvidenceKey: evidenceKey, SourceVersion: "source-v1",
					Supported: status.supported, Reason: status.reason,
				}}), nil)
	}

	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID, personfacts.EvidenceFilter{})
	require.NoError(err)
	require.Len(evidence, 1)
	assert.True(evidence[0].Supported)
	require.NotNil(evidence[0].LatestStatus)
	assert.Equal(personfacts.EvidenceStatusScopeRelinked, evidence[0].LatestStatus.Reason)
}

func TestPersonFactEvidenceStatusReasonsCoverDeleteEditUnlinkAndIdentityReassignment(t *testing.T) {
	st, personID := newPersonFactLedgerStore(t)
	evidenceKey := seedPersonFactEvidence(t, st, personID, "reasons")
	reasons := []personfacts.EvidenceStatusReason{
		personfacts.EvidenceStatusSourceDeleted,
		personfacts.EvidenceStatusSourceEdited,
		personfacts.EvidenceStatusScopeUnlinked,
		personfacts.EvidenceStatusIdentityReassigned,
	}
	for index, reason := range reasons {
		persistPersonFactLedgerGeneration(t, st,
			preparePersonFactLedgerGeneration(t, personID, "reason-"+string(rune('a'+index)), nil,
				[]personfacts.EvidenceStatusChange{{
					EvidenceKey: evidenceKey, SourceVersion: "source-v1",
					Supported: false, Reason: reason,
				}}), nil)
	}

	events, err := st.ListPersonFactEvidenceStatusEventsContext(t.Context(), personID,
		personfacts.EvidenceStatusFilter{EvidenceKey: evidenceKey})
	require.NoError(t, err)
	got := make([]personfacts.EvidenceStatusReason, 0, len(events))
	for _, event := range events {
		got = append(got, event.Reason)
	}
	assert.ElementsMatch(t, reasons, got)
}

func TestPersonFactEvidenceStatusReplayIsIdempotent(t *testing.T) {
	st, personID := newPersonFactLedgerStore(t)
	evidenceKey := seedPersonFactEvidence(t, st, personID, "status-replay")
	prepared := preparePersonFactLedgerGeneration(t, personID, "status-replay-generation", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceEdited,
		}})
	first := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	second := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	assert.Equal(t, first, second)
	assert.Len(t, second.EvidenceStatusEvents, 1)
}

func TestPersonFactEvidenceStatusRejectsCanonicalKeyCollision(t *testing.T) {
	st, personID := newPersonFactLedgerStore(t)
	evidenceKey := seedPersonFactEvidence(t, st, personID, "status-collision")
	prepared := preparePersonFactLedgerGeneration(t, personID, "status-collision-generation", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	require.Len(t, stored.EvidenceStatusEvents, 1)
	_, err := st.db.ExecContext(t.Context(),
		`UPDATE person_fact_evidence_status_events SET supported = TRUE WHERE id = ?`,
		stored.EvidenceStatusEvents[0].ID)
	require.NoError(t, err)

	err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
		generation, _, insertErr := st.insertPersonFactGenerationTx(t.Context(), tx, prepared)
		if insertErr != nil {
			return insertErr
		}
		insertErr = st.insertPersonFactEvidenceStatusEventsTx(t.Context(), tx, generation,
			prepared.EvidenceStatusChanges())
		return insertErr
	})
	assert.ErrorIs(t, err, ErrPersonFactKeyCollision)
}

func TestPersonFactLedgerPersonDeletionCascadesAllRows(t *testing.T) {
	require := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	prepared := preparePersonFactLedgerGeneration(t, personID, "cascade", []personfacts.ProposedClaim{
		personFactLedgerClaim(personID, "favorite-color", `"blue"`, "cascade"),
	}, nil)
	claimKey, err := personfacts.ClaimKey(prepared.GenerationKey(), prepared.Claims()[0])
	require.NoError(err)
	resolution := personfacts.Resolution{
		Target:          personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "favorite-color", Revision: "revision-1"},
		ResolverVersion: personfacts.ResolverVersionV1, InputFingerprint: strings.Repeat("c", 64),
		ResolvedAt: personFactLedgerNow,
		Decisions: []personfacts.Decision{{
			ClaimKey: claimKey, Action: personfacts.DecisionRetained,
			Reason: personfacts.ReasonBelowThreshold,
		}},
	}
	stored := persistPersonFactLedgerGeneration(t, st, prepared, []personfacts.Resolution{resolution})
	evidenceKey := stored.Claims[0].EvidenceIDs[0]
	_ = evidenceKey
	statusPrepared := preparePersonFactLedgerGeneration(t, personID, "cascade-status", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: prepared.Claims()[0].EvidenceKeys[0], SourceVersion: "source-v1",
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	persistPersonFactLedgerGeneration(t, st, statusPrepared, nil)
	require.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
		_, insertErr := tx.ExecContext(t.Context(), `
			INSERT INTO person_fact_pin_events
				(person_id, target_kind, target_key, target_revision, pinned, actor)
			VALUES (?, 'attribute', 'favorite-color', 'revision-1', TRUE, 'fixture')`, personID)
		return insertErr
	}))

	var revision int64
	require.NoError(st.db.QueryRowContext(t.Context(),
		`SELECT revision FROM persons WHERE id = ?`, personID).Scan(&revision))
	require.NoError(st.DeletePersonContext(t.Context(), personID, revision))
	for _, table := range []string{
		"person_fact_generations", "person_fact_claims", "person_fact_claim_evidence",
		"person_fact_evidence", "person_fact_evidence_status_events",
		"person_fact_resolutions", "person_fact_decisions", "person_fact_pin_events",
	} {
		var count int
		require.NoError(st.db.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM `+table).Scan(&count), table)
		assert.Zero(t, count, table)
	}
}

func TestPersonFactLedgerPaginationValidationAndOrdering(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	for _, suffix := range []string{"a", "b", "c"} {
		prepared := preparePersonFactLedgerGeneration(t, personID, "page-"+suffix,
			[]personfacts.ProposedClaim{personFactLedgerClaim(personID, "target-"+suffix, `"value"`, suffix)}, nil)
		claimKey, err := personfacts.ClaimKey(prepared.GenerationKey(), prepared.Claims()[0])
		requirements.NoError(err)
		persistPersonFactLedgerGeneration(t, st, prepared, []personfacts.Resolution{{
			Target:          personfacts.TargetRef{Kind: personfacts.TargetAttribute, Key: "target-" + suffix, Revision: "revision-1"},
			ResolverVersion: personfacts.ResolverVersionV1, InputFingerprint: strings.Repeat(suffix, 64),
			ResolvedAt: personFactLedgerNow,
			Decisions: []personfacts.Decision{{
				ClaimKey: claimKey, Action: personfacts.DecisionRetained,
				Reason: personfacts.ReasonBelowThreshold,
			}},
		}})
		persistPersonFactLedgerGeneration(t, st,
			preparePersonFactLedgerGeneration(t, personID, "page-status-"+suffix, nil,
				[]personfacts.EvidenceStatusChange{{
					EvidenceKey: prepared.Claims()[0].EvidenceKeys[0], SourceVersion: "source-v1",
					Supported: false, Reason: personfacts.EvidenceStatusSourceEdited,
				}}), nil)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "evidence limit", call: func() error {
			_, err := st.ListPersonFactEvidenceContext(t.Context(), personID, personfacts.EvidenceFilter{Limit: 201})
			return err
		}},
		{name: "status negative offset", call: func() error {
			_, err := st.ListPersonFactEvidenceStatusEventsContext(t.Context(), personID, personfacts.EvidenceStatusFilter{Offset: -1})
			return err
		}},
		{name: "claim limit", call: func() error {
			_, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{Limit: -1})
			return err
		}},
		{name: "decision limit", call: func() error {
			_, err := st.ListPersonFactDecisionsContext(t.Context(), personID, personfacts.DecisionFilter{Limit: 500})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			assert.Error(test.call())
		})
	}

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID,
		personfacts.ClaimFilter{Limit: 2, Offset: 1})
	requirements.NoError(err)
	requirements.Len(claims, 2)
	assertions.Greater(claims[0].ID, claims[1].ID)
	decisions, err := st.ListPersonFactDecisionsContext(t.Context(), personID,
		personfacts.DecisionFilter{})
	requirements.NoError(err)
	requirements.Len(decisions, 3)
	assertions.True(sort.SliceIsSorted(decisions, func(i, j int) bool { return decisions[i].ID > decisions[j].ID }))
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{})
	requirements.NoError(err)
	requirements.Len(evidence, 3)
	assertions.True(sort.SliceIsSorted(evidence, func(i, j int) bool { return evidence[i].ID > evidence[j].ID }))
	statuses, err := st.ListPersonFactEvidenceStatusEventsContext(t.Context(), personID,
		personfacts.EvidenceStatusFilter{})
	requirements.NoError(err)
	requirements.Len(statuses, 3)
	assertions.True(sort.SliceIsSorted(statuses, func(i, j int) bool { return statuses[i].ID > statuses[j].ID }))
	unsupported := false
	statuses, err = st.ListPersonFactEvidenceStatusEventsContext(t.Context(), personID,
		personfacts.EvidenceStatusFilter{Supported: &unsupported})
	requirements.NoError(err)
	assertions.Len(statuses, 3)
	supported := true
	statuses, err = st.ListPersonFactEvidenceStatusEventsContext(t.Context(), personID,
		personfacts.EvidenceStatusFilter{Supported: &supported})
	requirements.NoError(err)
	assertions.NotNil(statuses)
	assertions.Empty(statuses)

	target := personfacts.TargetRef{
		Kind: personfacts.TargetAttribute, Key: "target-b", Revision: "revision-1",
	}
	targetEvidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Target: &target})
	requirements.NoError(err)
	assertions.Len(targetEvidence, 1)
	targetClaims, err := st.ListPersonFactClaimsContext(t.Context(), personID,
		personfacts.ClaimFilter{Target: &target})
	requirements.NoError(err)
	assertions.Len(targetClaims, 1)
	targetDecisions, err := st.ListPersonFactDecisionsContext(t.Context(), personID,
		personfacts.DecisionFilter{Target: &target})
	requirements.NoError(err)
	assertions.Len(targetDecisions, 1)

	empty, err := st.ListPersonFactEvidenceStatusEventsContext(t.Context(), personID+999,
		personfacts.EvidenceStatusFilter{})
	requirements.NoError(err)
	assertions.NotNil(empty)
	assertions.Empty(empty)
}

func newPersonFactLedgerStore(t *testing.T) (*Store, int64) {
	t.Helper()
	var st *Store
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if IsPostgresURL(testDB) {
		st = newPGStoreInternal(t, testDB)
	} else {
		var err error
		st, err = OpenForTest(filepath.Join(t.TempDir(), "fact-ledger.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = st.Close() })
		require.NoError(t, st.InitSchema())
	}
	participantID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	return st, person.ID
}

func preparePersonFactLedgerGeneration(
	t *testing.T,
	personID int64,
	suffix string,
	claims []personfacts.ProposedClaim,
	statuses []personfacts.EvidenceStatusChange,
) personfacts.PreparedGeneration {
	t.Helper()
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(),
		personFactLedgerGenerationInput(personID, suffix, claims, statuses), nil)
	require.NoError(t, err)
	return prepared
}

func personFactLedgerGenerationInput(
	personID int64,
	suffix string,
	claims []personfacts.ProposedClaim,
	statuses []personfacts.EvidenceStatusChange,
) personfacts.GenerationInput {
	return personfacts.GenerationInput{
		PersonID:      personID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: suffix, End: suffix + "-end"}},
		ProgramID:     "fact-ledger-fixture", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "catalog-v1",
		Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: personFactLedgerNow,
		Policy: personfacts.PolicyContext{
			ProviderPolicyFingerprint: "policy-v1",
		},
		Claims: claims, EvidenceStatusChanges: statuses,
	}
}

func personFactLedgerClaim(personID int64, targetKey, submitted, sourceSuffix string) personfacts.ProposedClaim {
	subject := personID
	return personfacts.ProposedClaim{
		Target: personfacts.TargetDescriptor{
			Kind: personfacts.TargetAttribute, Key: targetKey, Revision: "revision-1",
			UniversalID: targetKey, Slug: targetKey, Description: "Synthetic fact target",
			ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
		},
		Relation: personfacts.RelationSupport, SubmittedValue: json.RawMessage(submitted),
		Evidence: []personfacts.EvidenceInput{{
			PersonID: personID, SourceClass: personfacts.EvidencePublic,
			Directness: personfacts.DirectOther, Authority: personfacts.AuthorityAuthoritative,
			SourceURL: "https://example.com/" + sourceSuffix, SubjectPersonID: &subject,
			SubjectRef: "person-fixture", Excerpt: "Synthetic evidence " + sourceSuffix,
			SourceVersion: "source-v1", EventTime: personFactLedgerNow.Add(-time.Hour),
			RecordedTime: personFactLedgerNow, IdentityScore: 950,
		}},
		Origin:     personfacts.OriginExtraction,
		Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
	}
}

func seedPersonFactEvidence(t *testing.T, st *Store, personID int64, suffix string) string {
	t.Helper()
	prepared := preparePersonFactLedgerGeneration(t, personID, "evidence-"+suffix,
		[]personfacts.ProposedClaim{personFactLedgerClaim(personID, "target-"+suffix, `"value"`, suffix)}, nil)
	stored := persistPersonFactLedgerGeneration(t, st, prepared, nil)
	require.Len(t, stored.Claims, 1)
	require.Len(t, stored.Claims[0].EvidenceIDs, 1)
	return prepared.Claims()[0].EvidenceKeys[0]
}

func persistPersonFactLedgerGeneration(
	t *testing.T,
	st *Store,
	prepared personfacts.PreparedGeneration,
	resolutions []personfacts.Resolution,
) personFactLedgerGeneration {
	t.Helper()
	var stored personFactLedgerGeneration
	err := st.withTxContext(t.Context(), func(tx *loggedTx) error {
		generation, replay, insertErr := st.insertPersonFactGenerationTx(t.Context(), tx, prepared)
		if insertErr != nil {
			return insertErr
		}
		if !replay {
			if _, insertErr = st.insertPersonFactClaimsTx(t.Context(), tx, generation, prepared.Claims()); insertErr != nil {
				return insertErr
			}
			if insertErr = st.insertPersonFactEvidenceStatusEventsTx(
				t.Context(), tx, generation, prepared.EvidenceStatusChanges()); insertErr != nil {
				return insertErr
			}
			for _, resolution := range resolutions {
				if _, insertErr = st.insertPersonFactResolutionTx(t.Context(), tx, generation,
					resolution, prepared.Input().Policy.ProviderPolicyFingerprint); insertErr != nil {
					return insertErr
				}
			}
		}
		stored, insertErr = st.loadPersonFactGenerationTx(
			t.Context(), tx, generation.PersonID, generation.GenerationKey)
		return insertErr
	})
	require.NoError(t, err)
	return stored
}
