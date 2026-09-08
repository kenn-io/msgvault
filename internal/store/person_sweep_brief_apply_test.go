package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

// stubBriefAligner accepts archive evidence whose source ref is in accept, so a
// brief apply test can exercise the pointer path without standing up a real
// message archive. rejected records what it refused.
type stubBriefAligner struct {
	rejectRefs map[string]struct{}
	seen       []string
}

func (a *stubBriefAligner) Align(
	_ context.Context, input personfacts.EvidenceInput,
) (personfacts.AlignmentResult, error) {
	a.seen = append(a.seen, input.SourceRef)
	if _, rejected := a.rejectRefs[input.SourceRef]; rejected {
		return personfacts.AlignmentResult{Failure: &personfacts.ValidationFailure{
			Action: personfacts.DecisionIdentityRejected,
			Reason: personfacts.ReasonUnalignedEvidence,
			Detail: "person sweep source is unavailable",
		}}, nil
	}
	return personfacts.AlignmentResult{Accepted: true, SourceVersion: input.SourceVersion,
		ContentSHA256: input.ContentSHA256}, nil
}

func briefApplyEvidence(personID int64, ref string) personfacts.EvidenceInput {
	subject := personID
	start, end := int64(0), int64(12)
	return personfacts.EvidenceInput{
		PersonID: personID, SourceClass: personfacts.EvidenceArchive,
		Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityOrdinary,
		SourceRef: ref, SubjectPersonID: &subject, SpanStart: &start, SpanEnd: &end,
		Excerpt: "synthetic brief evidence", ContentSHA256: strings.Repeat("f", 64),
		SourceVersion: "source-v1", EventTime: personFactLedgerNow.Add(-time.Hour),
		RecordedTime: personFactLedgerNow, IdentityScore: 950,
	}
}

func briefApplyResult(personID int64, refs ...string) *peoplesweep.BriefResult {
	evidence := make([]personfacts.EvidenceInput, 0, len(refs))
	for _, ref := range refs {
		evidence = append(evidence, briefApplyEvidence(personID, ref))
	}
	structured := json.RawMessage(`{"last_meaningful_interaction":null,"highlights":[],` +
		`"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]}`)
	return &peoplesweep.BriefResult{
		ProgramID: peoplesweep.BriefProgramID, ProgramVersion: peoplesweep.BriefProgramVersion,
		ProgramFingerprint: peoplesweep.BriefProgramFingerprint(),
		Boundary: peoplesweep.BriefBoundary{
			Lanes:            []string{"conversation_text"},
			FromEventTime:    personFactLedgerNow.Add(-48 * time.Hour),
			ThroughEventTime: personFactLedgerNow.Add(-time.Hour),
			ThroughSequence:  4200, ItemCount: len(refs), InputBytes: 1024,
			PacketSHA256: strings.Repeat("a", 64),
		},
		Structured: structured,
		Rendered: peoplesweep.RenderedBrief{
			Policy: peoplesweep.BriefRendererPolicyV1,
			Text:   "Last time you talked (Aug 22): they start a new role.",
			Sentences: []peoplesweep.RenderedSentence{{
				Kind: "last_interaction", Index: 0,
				Text: "Last time you talked (Aug 22): they start a new role."}},
		},
		Evidence: evidence, DroppedItemCount: 2, GeneratedAt: personFactLedgerNow,
	}
}

// reserveBriefCall adds a started brief reservation at the ordinal after the
// fixture's extraction batch and returns the completed batch that applies it.
func reserveBriefCall(
	t *testing.T, f personSweepApplyFixture, hash string,
) peoplesweep.CompletedBatch {
	t.Helper()
	const callOrdinal = 0
	purpose := peoplesweep.ProviderCallPurposeBrief
	request := f.reservation.Request
	request.BatchOrdinal = 1
	request.CallOrdinal = callOrdinal
	request.Purpose = purpose
	request.InputHash = hash
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, f.lease))
	return peoplesweep.CompletedBatch{Ordinal: 1, CallOrdinal: callOrdinal, Purpose: purpose,
		ReservationID: reservation.ID, InputHash: hash, ProviderRequestID: "request-brief",
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
		Usage:      peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1},
		UsageKnown: true, ActualCostMicroUSD: 3, Latency: time.Second}
}

func addBriefUsage(usage peoplesweep.Usage, reserved peoplesweep.Usage) peoplesweep.Usage {
	return peoplesweep.Usage{Requests: usage.Requests + reserved.Requests,
		InputTokens:           usage.InputTokens + reserved.InputTokens,
		OutputTokens:          usage.OutputTokens + reserved.OutputTokens,
		EstimatedCostMicroUSD: usage.EstimatedCostMicroUSD + reserved.EstimatedCostMicroUSD}
}

func TestApplyPersonSweepStoresBriefWithExtractionBatches(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "brief-with-extraction", true)
	brief := reserveBriefCall(t, f, strings.Repeat("1", 64))
	f.request.Batches = append(f.request.Batches, brief)
	f.request.Usage = addBriefUsage(f.request.Usage, peoplesweep.Usage{Requests: 1,
		InputTokens: 3, OutputTokens: 2, EstimatedCostMicroUSD: 5})
	f.request.Brief = briefApplyResult(f.personID, "person-sweep/v1:brief-a", "person-sweep/v1:brief-b")

	aligner := &stubBriefAligner{}
	result, err := f.store.applyPersonSweepWithAligner(t.Context(), f.request, aligner)
	requirements.NoError(err)
	checks.Equal(1, result.Mutations.BriefVersion)
	checks.Equal(2, result.Mutations.BriefEvidenceRowsInserted)
	checks.Equal(2, result.Mutations.BatchRowsReconciled)

	stored, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	requirements.NoError(err)
	checks.Equal(1, stored.Version)
	checks.Equal(PersonBriefStatusCurrent, stored.Status)
	checks.Equal(peoplesweep.BriefProgramID, stored.ProgramID)
	checks.Equal("fixture-provider", stored.Provider)
	checks.Equal("provider-v1", stored.ProviderVersion)
	checks.Equal("fixture-model", stored.Model)
	checks.Equal("model-v1", stored.ModelVersion)
	checks.Equal(2, stored.DroppedItemCount)
	checks.Equal(peoplesweep.BriefRendererPolicyV1, stored.RendererPolicy)
	checks.Equal(f.request.Generation.PersonID, stored.PersonID)

	var boundary peoplesweep.BriefBoundary
	requirements.NoError(json.Unmarshal(stored.Boundary, &boundary))
	checks.Equal(int64(4200), boundary.ThroughSequence)

	pointers, err := f.store.ListPersonBriefEvidenceContext(t.Context(), stored.ID)
	requirements.NoError(err)
	requirements.Len(pointers, 2)
	checks.Equal([]int{0, 1}, []int{pointers[0].Ordinal, pointers[1].Ordinal})
	checks.Equal("person-sweep/v1:brief-a", pointers[0].SourceRef,
		"pointers are written in citation order")
	checks.Equal("person-sweep/v1:brief-b", pointers[1].SourceRef)
	checks.True(pointers[0].Supported)
	checks.Equal([]string{"person-sweep/v1:brief-a", "person-sweep/v1:brief-b"}, aligner.seen,
		"brief evidence goes through the same aligner claim evidence uses")

	var attemptBriefClass string
	requirements.NoError(f.store.db.QueryRow(
		`SELECT brief_failure_class FROM person_sweep_attempts WHERE id = ?`,
		f.attemptID).Scan(&attemptBriefClass))
	checks.Empty(attemptBriefClass)
}

func TestApplyPersonSweepDropsBriefEvidenceThatNoLongerAligns(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "brief-unaligned-evidence", true)
	brief := reserveBriefCall(t, f, strings.Repeat("2", 64))
	f.request.Batches = append(f.request.Batches, brief)
	f.request.Usage = addBriefUsage(f.request.Usage, peoplesweep.Usage{Requests: 1,
		InputTokens: 3, OutputTokens: 2, EstimatedCostMicroUSD: 5})
	f.request.Brief = briefApplyResult(f.personID, "person-sweep/v1:gone", "person-sweep/v1:kept")

	aligner := &stubBriefAligner{rejectRefs: map[string]struct{}{"person-sweep/v1:gone": {}}}
	result, err := f.store.applyPersonSweepWithAligner(t.Context(), f.request, aligner)
	requirements.NoError(err, "an unalignable citation must not fail the attempt")
	checks.Equal(1, result.Mutations.BriefEvidenceRowsInserted)

	stored, err := f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	requirements.NoError(err)
	pointers, err := f.store.ListPersonBriefEvidenceContext(t.Context(), stored.ID)
	requirements.NoError(err)
	requirements.Len(pointers, 1)
	checks.Equal("person-sweep/v1:kept", pointers[0].SourceRef)
}

func TestApplyPersonSweepRecordsBriefFailureAndReconcilesItsReservation(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "brief-failure", true)
	// The brief call started but never produced a usable response, so it is not
	// among the applied batches.
	_ = reserveBriefCall(t, f, strings.Repeat("3", 64))
	f.request.BriefFailureClass = peoplesweep.FailureInvalidOutput
	f.request.BriefRetryAt = f.request.CompletedAt.Add(time.Hour)

	result, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	requirements.NoError(err, "a brief failure must not roll back the extraction")
	checks.Equal(1, result.Mutations.AttemptRowsSucceeded)
	checks.Zero(result.Mutations.BriefVersion)

	var status, failureClass string
	requirements.NoError(f.store.db.QueryRow(`SELECT status, failure_class
		FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = 1 AND call_ordinal = 0`,
		f.attemptID).Scan(&status, &failureClass))
	checks.Equal("failed", status)
	checks.Equal(string(peoplesweep.FailureInvalidOutput), failureClass)

	var attemptStatus, attemptBriefClass string
	requirements.NoError(f.store.db.QueryRow(`SELECT status, brief_failure_class
		FROM person_sweep_attempts WHERE id = ?`, f.attemptID).Scan(&attemptStatus, &attemptBriefClass))
	checks.Equal("succeeded", attemptStatus)
	checks.Equal(string(peoplesweep.FailureInvalidOutput), attemptBriefClass)

	_, err = f.store.GetPersonBriefContext(t.Context(), f.personID, 0)
	requirements.ErrorIs(err, ErrPersonBriefNotFound)
}

func TestApplyPersonSweepStoresBriefOnStatusOnlyExtraction(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "brief-status-only", true)
	// Drop the extraction batch: only the brief call completed. The fixture
	// already marked its reservation started, so the row is removed directly
	// rather than released.
	f.request.Batches = nil
	f.request.Generation.Claims = nil
	_, err := f.store.db.ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = 0`), f.attemptID)
	requirements.NoError(err)

	briefRequest := f.reservation.Request
	briefRequest.BatchOrdinal = 0
	briefRequest.Purpose = peoplesweep.ProviderCallPurposeBrief
	briefRequest.InputHash = strings.Repeat("4", 64)
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(), briefRequest)
	requirements.NoError(err)
	requirements.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, f.lease))
	f.request.Batches = []peoplesweep.CompletedBatch{{Ordinal: 0, CallOrdinal: 0,
		Purpose: peoplesweep.ProviderCallPurposeBrief, ReservationID: reservation.ID,
		InputHash: briefRequest.InputHash, ProviderRequestID: "request-brief",
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
		Usage:      peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1},
		UsageKnown: true, ActualCostMicroUSD: 3, Latency: time.Second}}
	f.request.Usage = peoplesweep.Usage{Requests: 1, InputTokens: 3, OutputTokens: 2,
		EstimatedCostMicroUSD: 5}
	f.request.Brief = briefApplyResult(f.personID, "person-sweep/v1:status-only")

	result, err := f.store.applyPersonSweepWithAligner(t.Context(), f.request, &stubBriefAligner{})
	requirements.NoError(err)
	checks.Equal(1, result.Mutations.BriefVersion)

	var provider, model string
	requirements.NoError(f.store.db.QueryRow(`SELECT provider, model FROM person_fact_generations
		WHERE id = ?`, result.Generation.GenerationID).Scan(&provider, &model))
	checks.Equal("fixture-provider", provider,
		"a completed brief call makes the generation a provider generation")
	checks.Equal("fixture-model", model)
}

func TestApplyPersonSweepStoresBriefOriginClaimsInTheSameGeneration(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepApplyFixture(t, "brief-origin-claims", true)
	brief := reserveBriefCall(t, f, strings.Repeat("5", 64))
	f.request.Batches = append(f.request.Batches, brief)
	f.request.Usage = addBriefUsage(f.request.Usage, peoplesweep.Usage{Requests: 1,
		InputTokens: 3, OutputTokens: 2, EstimatedCostMicroUSD: 5})
	f.request.Brief = briefApplyResult(f.personID, "person-sweep/v1:claim-evidence")
	briefClaim := personFactProjectionClaim(f.personID,
		f.targets[AttributeSlugPrimaryChannel], `"email"`, "brief-origin")
	briefClaim.Origin = personfacts.OriginBrief
	f.request.Generation.Claims = append(f.request.Generation.Claims, briefClaim)

	result, err := f.store.applyPersonSweepWithAligner(t.Context(), f.request, &stubBriefAligner{})
	requirements.NoError(err)

	var briefOrigins int
	requirements.NoError(f.store.db.QueryRow(`SELECT COUNT(*) FROM person_fact_claims
		WHERE generation_id = ? AND origin = 'brief'`,
		result.Generation.GenerationID).Scan(&briefOrigins))
	checks.Equal(1, briefOrigins,
		"a brief's proposed attribute is recorded with its own origin in the shared generation")
}

func TestApplyPersonSweepRejectsInconsistentBriefRequests(t *testing.T) {
	for name, mutate := range map[string]func(*personSweepApplyFixture){
		"brief without a completed brief call": func(f *personSweepApplyFixture) {
			f.request.Brief = briefApplyResult(f.personID, "person-sweep/v1:orphan")
		},
		"brief and a brief failure class together": func(f *personSweepApplyFixture) {
			f.request.Brief = briefApplyResult(f.personID, "person-sweep/v1:orphan")
			f.request.BriefFailureClass = peoplesweep.FailureInvalidOutput
			f.request.BriefRetryAt = f.request.CompletedAt.Add(time.Hour)
		},
		"brief failure without retry time": func(f *personSweepApplyFixture) {
			f.request.BriefFailureClass = peoplesweep.FailureInvalidOutput
		},
		"retry time without brief failure": func(f *personSweepApplyFixture) {
			f.request.BriefRetryAt = f.request.CompletedAt.Add(time.Hour)
		},
		"unknown brief failure class": func(f *personSweepApplyFixture) {
			f.request.BriefFailureClass = peoplesweep.FailureClass("nonsense")
		},
		"brief for another person": func(f *personSweepApplyFixture) {
			f.request.Brief = briefApplyResult(f.personID+1000, "person-sweep/v1:other")
		},
		"brief without a rendered paragraph": func(f *personSweepApplyFixture) {
			brief := briefApplyResult(f.personID, "person-sweep/v1:blank")
			brief.Rendered.Text = ""
			f.request.Brief = brief
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPersonSweepApplyFixture(t, "brief-invalid-"+strings.ReplaceAll(name, " ", "-"), true)
			mutate(&f)
			_, err := f.store.applyPersonSweepWithAligner(t.Context(), f.request, &stubBriefAligner{})
			require.Error(t, err)
		})
	}
}

func TestApplyPersonSweepRefusesUncoveredBriefCallWithoutAFailureClass(t *testing.T) {
	f := newPersonSweepApplyFixture(t, "brief-uncovered", true)
	_ = reserveBriefCall(t, f, strings.Repeat("6", 64))

	_, err := f.store.ApplyPersonSweep(t.Context(), f.request)
	require.ErrorContains(t, err, "do not exactly cover durable reservations")
}
