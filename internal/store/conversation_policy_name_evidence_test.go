package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Name authority comes from the captured message envelope, not room membership.
func policyNameEnvelope(t *testing.T, s *store.Store, source, room int64, key string, participants []int64, names []string) {
	t.Helper()
	message, err := s.UpsertMessage(&store.Message{SourceID: source, ConversationID: room, SourceMessageID: key, MessageType: "chat"})
	require.NoError(t, err)
	require.NoError(t, s.ReplaceMessageRecipients(message, "to", participants, names))
}

func TestConversationPolicyNameEvidencePreservesExactValueAndUnicode(t *testing.T) {
	for _, drift := range []bool{false, true} {
		name := "unicode-case-fold"
		if drift {
			name = "persisted-detail-drift"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			ctx := t.Context()
			s := testutil.NewTestStore(t)
			s.DB().SetMaxOpenConns(1)
			source, err := s.GetOrCreateSource("beeper", "name-value-authority")
			requirements.NoError(err)
			original, err := s.EnsureConversation(source.ID, "original", "Original names")
			requirements.NoError(err)
			alternate, err := s.EnsureConversation(source.ID, "alternate", "Alternate support")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "original", conversationpolicy.Full))
			requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "alternate", conversationpolicy.Full))
			left, err := s.EnsureParticipantByIdentifier("beeper", "value-left", "Åda Example")
			requirements.NoError(err)
			right, err := s.EnsureParticipantByIdentifier("beeper", "value-right", "åDA EXAMPLE")
			requirements.NoError(err)
			ids := []int64{left, right}
			policyNameEnvelope(t, s, source.ID, original, "original-name", ids, []string{"Åda Example", "åDA EXAMPLE"})
			alternateNames := []string{"Åda Example", "åDA EXAMPLE"}
			if drift {
				alternateNames = []string{"New Example", "New Example"}
			}
			policyNameEnvelope(t, s, source.ID, alternate, "alternate-name", ids, alternateNames)
			var origin int64
			for _, id := range ids {
				observation, err := s.RecordContactObservationContext(ctx, id, store.ParticipantContactObservationInput{SourceID: &source.ID, ConversationID: &alternate, AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
				requirements.NoError(err)
				origin = observation.Observation.Envelope.ID
			}
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: left, RightKind: store.IdentityMatchParticipant, RightID: right, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
			requirements.NoError(err)
			revision, err := s.ConversationPolicyRevision(ctx)
			requirements.NoError(err)
			automatic, err := s.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "display_name", Detail: new("Åda Example"), Source: store.ProvenanceArchiveObservation, SourceID: &source.ID, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin}})
			requirements.NoError(err)
			manual, err := s.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "display_name", Detail: new("Åda Example"), Source: store.ProvenanceUser})
			requirements.NoError(err)
			if drift {
				_, err := s.DB().ExecContext(ctx, s.Rebind(`UPDATE participants SET display_name=? WHERE id IN (?,?)`), "New Example", left, right)
				requirements.NoError(err)
			}
			requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "original", conversationpolicy.Cold))
			kept, err := s.GetIdentityMatchCandidateContext(ctx, candidate.ID)
			requirements.NoError(err)
			byID := map[int64]store.IdentityMatchEvidence{}
			for _, evidence := range kept.Evidence {
				byID[evidence.ID] = evidence
			}
			_, automaticKept := byID[automatic.ID]
			assertions.Equal(!drift, automaticKept)
			requirements.Contains(byID, manual.ID)
			requirements.NotNil(byID[manual.ID].Detail)
			assertions.Equal("Åda Example", *byID[manual.ID].Detail)
		})
	}
}

// Withdrawing the only envelope behind generated name evidence must keep the
// review page readable and must retire tokens issued while the name was shown.
func TestConversationPolicyNameWithdrawalRetiresReviewToken(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	ctx := t.Context()
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "name-withdrawal-token")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "room", "Only name support")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "room", conversationpolicy.Full))
	left, err := s.EnsureParticipantByIdentifier("beeper", "withdraw-left", "Rae Example")
	requirements.NoError(err)
	right, err := s.EnsureParticipantByIdentifier("beeper", "withdraw-right", "Rae Example")
	requirements.NoError(err)
	ids := []int64{left, right}
	policyNameEnvelope(t, s, source.ID, room, "name", ids, []string{"Rae Example", "Rae Example"})
	var origin int64
	for _, id := range ids {
		observation, err := s.RecordContactObservationContext(ctx, id, store.ParticipantContactObservationInput{SourceID: &source.ID, ConversationID: &room, AddressKind: store.ContactAddressEmail, OriginalValue: "rae@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
		requirements.NoError(err)
		origin = observation.Observation.Envelope.ID
	}
	candidate, _, err := s.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: left, RightKind: store.IdentityMatchParticipant, RightID: right, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation})
	requirements.NoError(err)
	revision, err := s.ConversationPolicyRevision(ctx)
	requirements.NoError(err)
	_, err = s.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "display_name", Detail: new("Rae Example"), Source: store.ProvenanceArchiveObservation, SourceID: &source.ID, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin}})
	requirements.NoError(err)

	reviewToken := func() string {
		t.Helper()
		listed, err := s.ListIdentityMatchReviewsContext(ctx, nil, 10, 0)
		requirements.NoError(err)
		for _, item := range listed {
			if item.ID == candidate.ID {
				return item.ReviewToken
			}
		}
		requirements.FailNow("candidate missing from review page")
		return ""
	}
	shown := reviewToken()
	requirements.NotEmpty(shown)

	policyNameEnvelope(t, s, source.ID, room, "name", ids, []string{"Other Example", "Other Example"})
	withdrawn := reviewToken()
	assertions.NotEqual(shown, withdrawn)
	single, err := s.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	requirements.NoError(err)
	assertions.Empty(single.Evidence)

	_, _, err = s.DecideIdentityMatchReviewedContext(ctx, candidate.ID, shown, store.IdentityMatchStateAccepted, nil)
	requirements.ErrorIs(err, store.ErrIdentityMatchReviewStale)
}
