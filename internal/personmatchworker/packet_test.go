package personmatchworker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/personmatchpolicy"
	"go.kenn.io/msgvault/internal/store"
)

func TestBuildPairPacketUsesOnlySupportedIdentityEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	value := "same@example.test"
	private := "private conversation and notes"
	candidate := store.IdentityMatchCandidate{
		ID: 7, LeftKind: store.IdentityMatchParticipant, LeftID: 11,
		RightKind: store.IdentityMatchParticipant, RightID: 12,
		Basis: store.IdentityMatchEmail, NormalizedValue: &value,
		State: store.IdentityMatchStateCandidate, Actionable: true,
		SourceSupport: []store.IdentityMatchSourceSupport{{SourceID: 1}, {SourceID: 2}},
		Evidence: []store.IdentityMatchEvidence{
			{EvidenceKind: "email", Detail: &value, SourceSupport: []store.IdentityMatchSourceSupport{{SourceID: 1}}},
			{EvidenceKind: "self_declaration", Detail: &private, SourceSupport: []store.IdentityMatchSourceSupport{{SourceID: 2}}},
			{EvidenceKind: "conversation", Detail: &private},
		},
	}
	packet, signals, err := BuildPairPacket(&candidate)
	require.NoError(err)
	assert.Equal("participant", packet.Left.Kind)
	assert.Equal("participant", packet.Right.Kind)
	require.Len(packet.Evidence, 2)
	assert.Equal(value, packet.Evidence[0].Value)
	assert.Empty(packet.Evidence[1].Value)
	assert.Equal(personmatchpolicy.SignalSelfDeclaration, signals[1].Class)
}

func TestLocalPolicyInputRecognizesSharedPhoneAndMergeBlocker(t *testing.T) {
	candidate := store.IdentityMatchCandidate{Blocker: "person_merge_required", Actionable: false,
		Evidence: []store.IdentityMatchEvidence{{EvidenceKind: "shared_phone"}}}
	input := localPolicyInput(candidate, store.PersonMatchPairSummaries{}, []personmatchpolicy.Signal{
		{Class: personmatchpolicy.SignalPhone, SourceID: 1},
		{Class: personmatchpolicy.SignalSelfDeclaration, SourceID: 2},
	})
	input.Probability, input.MinimumProbability = 0.99, 0.80
	decision := personmatchpolicy.Evaluate(input)
	assert.Equal(t, personmatchpolicy.NeedsReview, decision.Action)
	assert.Contains(t, decision.Blockers, "shared_contact_point")
	assert.Contains(t, decision.Blockers, "manual_merge_review")
}

func TestBuildPairPacketDoesNotCountConservativeSupportAsIndependent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	value := "same@example.test"
	candidate := store.IdentityMatchCandidate{
		LeftKind: store.IdentityMatchParticipant, LeftID: 11,
		RightKind: store.IdentityMatchParticipant, RightID: 12,
		Basis: store.IdentityMatchEmail, NormalizedValue: &value,
		State: store.IdentityMatchStateCandidate, Actionable: true,
		Evidence: []store.IdentityMatchEvidence{
			{EvidenceKind: "email", SourceSupport: []store.IdentityMatchSourceSupport{{SourceID: 1}, {SourceID: 2, IsConservative: true}}},
			{EvidenceKind: "self_declaration", SourceSupport: []store.IdentityMatchSourceSupport{{SourceID: 2, IsConservative: true}}},
		},
	}
	packet, signals, err := BuildPairPacket(&candidate)
	require.NoError(err)
	assert.Equal(1, packet.Evidence[0].SourceCount)
	assert.Equal(0, packet.Evidence[1].SourceCount)
	decision := personmatchpolicy.Evaluate(personmatchpolicy.Input{Probability: 0.99, MinimumProbability: 0.8, Signals: signals})
	assert.Equal(personmatchpolicy.NeedsReview, decision.Action)
	assert.Contains(decision.Blockers, "independent_identity_evidence_required")
}

func TestLocalPolicyInputChecksPairSummaryGuards(t *testing.T) {
	candidate := store.IdentityMatchCandidate{Actionable: true}
	summaries := store.PersonMatchPairSummaries{
		Left:  personmatch.PairEndpoint{Phone: "+15550001111", Identifiers: []personmatch.PairIdentifier{{Type: "beeper", Value: "left"}}},
		Right: personmatch.PairEndpoint{Phone: "+15550001111", Identifiers: []personmatch.PairIdentifier{{Type: "beeper", Value: "right"}}},
	}
	input := localPolicyInput(candidate, summaries, []personmatchpolicy.Signal{
		{Class: personmatchpolicy.SignalEmail, SourceID: 1},
		{Class: personmatchpolicy.SignalSelfDeclaration, SourceID: 2},
	})
	input.Probability, input.MinimumProbability = 0.99, 0.8
	decision := personmatchpolicy.Evaluate(input)
	assert.Equal(t, personmatchpolicy.NeedsReview, decision.Action)
	assert.Contains(t, decision.Blockers, "conflicting_stable_id")
	summaries.Right.Identifiers[0].ScopeValue = "other-account"
	input = localPolicyInput(candidate, summaries, input.Signals)
	input.Probability, input.MinimumProbability = 0.99, 0.8
	assert.Equal(t, personmatchpolicy.ProposedAccept, personmatchpolicy.Evaluate(input).Action,
		"a matching phone is positive evidence and different stable-ID scopes do not conflict")
}

func TestLocalPolicyInputVerifiesExactFullName(t *testing.T) {
	summaries := store.PersonMatchPairSummaries{
		Left:  personmatch.PairEndpoint{DisplayName: "Ada Lovelace"},
		Right: personmatch.PairEndpoint{DisplayName: "ada  lovelace"},
	}
	input := localPolicyInput(store.IdentityMatchCandidate{Actionable: true}, summaries, []personmatchpolicy.Signal{
		{Class: personmatchpolicy.SignalDisplayName, SourceID: 1},
		{Class: personmatchpolicy.SignalEmail, SourceID: 2},
	})
	input.Probability, input.MinimumProbability = 0.91, 0.8
	assert.Equal(t, personmatchpolicy.ProposedAccept, personmatchpolicy.Evaluate(input).Action)
	summaries.Right.DisplayName = "Ada Byron"
	input = localPolicyInput(store.IdentityMatchCandidate{Actionable: true}, summaries, []personmatchpolicy.Signal{
		{Class: personmatchpolicy.SignalDisplayName, SourceID: 1},
		{Class: personmatchpolicy.SignalEmail, SourceID: 2},
	})
	input.Probability, input.MinimumProbability = 0.91, 0.8
	assert.Equal(t, personmatchpolicy.NeedsReview, personmatchpolicy.Evaluate(input).Action)
}
