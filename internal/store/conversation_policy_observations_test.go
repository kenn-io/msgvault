package store_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func policyObservationFixture(t *testing.T) (*store.Store, int64, int64, int64, int64) {
	t.Helper()
	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	_, err := s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	require.NoError(t, err)
	source, err := s.GetOrCreateSource("beeper", "observation-account")
	require.NoError(t, err)
	full, err := s.EnsureConversation(source.ID, "full", "Full")
	require.NoError(t, err)
	cold, err := s.EnsureConversation(source.ID, "cold", "Cold")
	require.NoError(t, err)
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source.ID, "full", conversationpolicy.Full))
	pid, err := s.EnsureParticipantByIdentifier("beeper", "synthetic-user", "Synthetic User")
	require.NoError(t, err)
	return s, source.ID, full, cold, pid
}

func policyObservationInput(source int64, room *int64) store.ParticipantContactObservationInput {
	return store.ParticipantContactObservationInput{SourceID: &source, ConversationID: room, AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.org", ProviderUserID: new("provider:synthetic-user"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
}

func TestColdProviderIDChangeDoesNotSupersedeFullObservation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, source, full, cold, participantID := policyObservationFixture(t)
	ctx := t.Context()
	otherParticipantID, err := s.EnsureParticipantByIdentifier(
		"beeper", "synthetic-other-user", "Synthetic Other User",
	)
	requirements.NoError(err)

	fullInput := policyObservationInput(source, &full)
	fullInput.ProviderUserID = new("provider-left")
	first, err := s.RecordContactObservationContext(ctx, participantID, fullInput)
	requirements.NoError(err)

	otherInput := policyObservationInput(source, &full)
	otherInput.ProviderUserID = new("provider-right")
	conflict, err := s.RecordContactObservationContext(ctx, otherParticipantID, otherInput)
	requirements.NoError(err)
	requirements.True(conflict.Conflicting)

	coldInput := policyObservationInput(source, &cold)
	coldInput.ProviderUserID = new("provider-right")
	result, err := s.RecordContactObservationContext(ctx, participantID, coldInput)
	requirements.NoError(err)

	assertions.False(result.Created,
		"a Cold observation cannot replace the same identity already supported by a Full room")
	assertions.Equal(first.Observation.Envelope.ID, result.Observation.Envelope.ID)
	current, err := s.ListParticipantObservationsContext(ctx, participantID, true)
	requirements.NoError(err)
	requirements.Len(current, 1)
	assertions.Equal("provider-left", *current[0].ProviderUserID)

	eligible, err := s.ObservationFullEligibleContext(ctx, first.Observation.Envelope.ID)
	requirements.NoError(err)
	assertions.True(eligible, "the Full room's identity evidence must stay eligible")
	candidates, err := s.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	requirements.NoError(err)
	assertions.Len(candidates, 1,
		"a Cold provider ID must not retract the identity conflict supported by Full observations")
}

func TestColdProviderIDDoesNotEnrichFullObservationWithoutProviderID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, source, full, cold, participantID := policyObservationFixture(t)

	fullInput := policyObservationInput(source, &full)
	fullInput.ProviderUserID = nil
	first, err := s.RecordContactObservationContext(t.Context(), participantID, fullInput)
	requirements.NoError(err)

	coldInput := policyObservationInput(source, &cold)
	coldProviderID := "provider:cold-only"
	coldInput.ProviderUserID = &coldProviderID
	result, err := s.RecordContactObservationContext(t.Context(), participantID, coldInput)
	requirements.NoError(err)

	assertions.True(result.Created,
		"a binding from a Cold room must remain separate from the Full-supported address row")
	assertions.NotEqual(first.Observation.Envelope.ID, result.Observation.Envelope.ID)
	assertions.Equal(coldProviderID, *result.Observation.ProviderUserID)
	current, err := s.ListParticipantObservationsContext(t.Context(), participantID, true)
	requirements.NoError(err)
	requirements.Len(current, 2)
	assertions.Nil(first.Observation.ProviderUserID)
	assertions.Equal([]int64{full}, policyObservationSupport(t, s, first.Observation.Envelope.ID))
	assertions.Equal([]int64{cold}, policyObservationSupport(t, s, result.Observation.Envelope.ID))
	eligible, err := s.ObservationFullEligibleContext(t.Context(), result.Observation.Envelope.ID)
	requirements.NoError(err)
	assertions.False(eligible, "the Cold binding cannot inherit the Full room's address support")
	found, err := s.FindObservationsByProviderUserIDContext(t.Context(), coldProviderID, 0)
	requirements.NoError(err)
	assertions.Empty(found, "Cold provider evidence cannot drive identity matching")
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
	found, err = s.FindObservationsByProviderUserIDContext(t.Context(), coldProviderID, 0)
	requirements.NoError(err)
	requirements.Len(found, 1, "promoting the original room makes its preserved binding eligible")
}

func TestParticipantMergeDoesNotPromoteColdProviderBinding(t *testing.T) {
	for _, survivorIsCold := range []bool{false, true} {
		name := "full survivor"
		if survivorIsCold {
			name = "cold survivor"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, source, full, cold, _ := policyObservationFixture(t)
			fullParticipant, err := s.EnsureParticipantByIdentifier(
				"beeper", "synthetic-full-observation", "Synthetic Full Observation",
			)
			requirements.NoError(err)
			coldParticipant, err := s.EnsureParticipantByIdentifier(
				"beeper", "synthetic-cold-observation", "Synthetic Cold Observation",
			)
			requirements.NoError(err)

			fullInput := policyObservationInput(source, &full)
			fullInput.ProviderUserID = nil
			_, err = s.RecordContactObservationContext(t.Context(), fullParticipant, fullInput)
			requirements.NoError(err)

			coldInput := policyObservationInput(source, &cold)
			coldInput.ProviderUserID = new("provider:cold-only")
			_, err = s.RecordContactObservationContext(t.Context(), coldParticipant, coldInput)
			requirements.NoError(err)

			survivorID, absorbedID := fullParticipant, coldParticipant
			if survivorIsCold {
				survivorID, absorbedID = coldParticipant, fullParticipant
			}
			requirements.NoError(s.MergeParticipantsContext(t.Context(), absorbedID, survivorID))

			current, err := s.ListParticipantObservationsContext(t.Context(), survivorID, true)
			requirements.NoError(err)
			requirements.Len(current, 2,
				"a participant merge must preserve observations with different provider bindings")
			var fullObservation, coldObservation *store.ParticipantContactObservation
			for i := range current {
				if current[i].ProviderUserID == nil {
					fullObservation = &current[i]
				} else if *current[i].ProviderUserID == "provider:cold-only" {
					coldObservation = &current[i]
				}
			}
			requirements.NotNil(fullObservation)
			requirements.NotNil(coldObservation)
			fullEligible, err := s.ObservationFullEligibleContext(t.Context(), fullObservation.Envelope.ID)
			requirements.NoError(err)
			assertions.True(fullEligible)
			coldEligible, err := s.ObservationFullEligibleContext(t.Context(), coldObservation.Envelope.ID)
			requirements.NoError(err)
			assertions.False(coldEligible,
				"a Cold-only provider binding must not inherit the separate Full observation's room support")
		})
	}
}

func TestParticipantMergePreservesIdenticalColdProviderBinding(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, source, _, firstCold, _ := policyObservationFixture(t)
	secondCold, err := s.EnsureConversation(source, "second-cold", "Second cold room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Cold))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "second-cold", conversationpolicy.Cold))

	first, err := s.EnsureParticipantByIdentifier("beeper", "synthetic-cold-merge-first", "First")
	requirements.NoError(err)
	second, err := s.EnsureParticipantByIdentifier("beeper", "synthetic-cold-merge-second", "Second")
	requirements.NoError(err)
	providerID := "provider:shared-cold-user"
	sourceRef := "beeper:synthetic-account:user"

	firstInput := policyObservationInput(source, &firstCold)
	firstInput.ProviderUserID = &providerID
	firstInput.Envelope.SourceRef = &sourceRef
	firstObservation, err := s.RecordContactObservationContext(t.Context(), first, firstInput)
	requirements.NoError(err)
	secondInput := policyObservationInput(source, &secondCold)
	secondInput.ProviderUserID = &providerID
	secondInput.Envelope.SourceRef = &sourceRef
	_, err = s.RecordContactObservationContext(t.Context(), second, secondInput)
	requirements.NoError(err)

	requirements.NoError(s.MergeParticipantsContext(t.Context(), second, first))
	current, err := s.ListParticipantObservationsContext(t.Context(), first, true)
	requirements.NoError(err)
	requirements.Len(current, 1, "identical provider evidence should collapse after the merge")
	requirements.NotNil(current[0].ProviderUserID)
	assertions.Equal(providerID, *current[0].ProviderUserID,
		"the binding must stay recorded while both supporting rooms are Cold")
	assertions.Equal([]int64{firstCold, secondCold}, policyObservationSupport(t, s, current[0].Envelope.ID))

	eligible, err := s.ObservationFullEligibleContext(t.Context(), current[0].Envelope.ID)
	requirements.NoError(err)
	assertions.False(eligible)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
	eligible, err = s.ObservationFullEligibleContext(t.Context(), current[0].Envelope.ID)
	requirements.NoError(err)
	assertions.True(eligible,
		"promoting the survivor's original room must restore the preserved provider binding")
	byProvider, err := s.FindObservationsByProviderUserIDContext(t.Context(), providerID, 0)
	requirements.NoError(err)
	requirements.Len(byProvider, 1)
	assertions.Equal(first, byProvider[0].ParticipantID)
	assertions.Equal(firstObservation.Observation.Envelope.ID, byProvider[0].Envelope.ID)
}

func TestProviderBindingEnrichmentReconcilesExistingObservationConflict(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, source, full, _, _ := policyObservationFixture(t)
	left, err := s.EnsureParticipantByIdentifier("beeper", "synthetic-enrichment-left", "Left")
	requirements.NoError(err)
	right, err := s.EnsureParticipantByIdentifier("beeper", "synthetic-enrichment-right", "Right")
	requirements.NoError(err)
	providerID := "provider:enriched-user"

	leftInput := policyObservationInput(source, &full)
	leftInput.ProviderUserID = &providerID
	leftInput.Envelope.SourceRef = new("beeper:synthetic-account:left")
	_, err = s.RecordContactObservationContext(t.Context(), left, leftInput)
	requirements.NoError(err)

	rightInput := policyObservationInput(source, &full)
	rightInput.ProviderUserID = nil
	rightInput.Envelope.SourceRef = new("beeper:synthetic-account:right")
	rightObservation, err := s.RecordContactObservationContext(t.Context(), right, rightInput)
	requirements.NoError(err)
	assertions.Nil(rightObservation.Observation.ProviderUserID)
	candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.Equal(store.IdentityMatchStateConflict, candidates[0].State)
	beforeRevision, err := s.ParticipantIdentifierRevisionContext(t.Context())
	requirements.NoError(err)

	rightInput.ProviderUserID = &providerID
	enriched, err := s.RecordContactObservationContext(t.Context(), right, rightInput)
	requirements.NoError(err)
	assertions.False(enriched.Created)
	assertions.Equal(rightObservation.Observation.Envelope.ID, enriched.Observation.Envelope.ID)
	requirements.NotNil(enriched.Observation.ProviderUserID)
	assertions.Equal(providerID, *enriched.Observation.ProviderUserID)
	afterRevision, err := s.ParticipantIdentifierRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(afterRevision, beforeRevision,
		"changing a provider binding must advance the participant projection revision")

	candidates, err = s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	for _, candidate := range candidates {
		assertions.NotEqual(store.IdentityMatchStateConflict, candidate.State,
			"the old address conflict must be reconciled when both observations acquire the same provider ID")
	}
}

func TestParticipantMergePreservesBeeperSourceRefBindings(t *testing.T) {
	for _, survivorIsCold := range []bool{false, true} {
		name := "full survivor"
		if survivorIsCold {
			name = "cold survivor"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s, source, full, cold, _ := policyObservationFixture(t)
			fullParticipant, err := s.EnsureParticipantByIdentifier(
				"beeper", "synthetic-full-source-ref-user", "Synthetic Full Source Ref User",
			)
			requirements.NoError(err)
			coldParticipant, err := s.EnsureParticipantByIdentifier(
				"beeper", "synthetic-cold-source-ref-user", "Synthetic Cold Source Ref User",
			)
			requirements.NoError(err)

			fullInput := policyObservationInput(source, &full)
			fullInput.Envelope.SourceRef = new("beeper:synthetic-full-account:user")
			fullInput.ProviderUserID = new("provider:shared-user")
			fullObservation, err := s.RecordContactObservationContext(t.Context(), fullParticipant, fullInput)
			requirements.NoError(err)

			coldInput := policyObservationInput(source, &cold)
			coldInput.Envelope.SourceRef = new("beeper:synthetic-cold-account:user")
			coldInput.ProviderUserID = new("provider:shared-user")
			coldObservation, err := s.RecordContactObservationContext(t.Context(), coldParticipant, coldInput)
			requirements.NoError(err)

			survivorID, absorbedID := fullParticipant, coldParticipant
			if survivorIsCold {
				survivorID, absorbedID = coldParticipant, fullParticipant
			}
			requirements.NoError(s.MergeParticipantsContext(t.Context(), absorbedID, survivorID))

			current, err := s.ListParticipantObservationsContext(t.Context(), survivorID, true)
			requirements.NoError(err)
			requirements.Len(current, 2,
				"observations from distinct Beeper source refs must not collapse on participant merge")
			bySourceRef := make(map[string]store.ParticipantContactObservation, len(current))
			for _, observation := range current {
				bySourceRef[*observation.Envelope.SourceRef] = observation
			}
			requirements.Contains(bySourceRef, "beeper:synthetic-full-account:user")
			requirements.Contains(bySourceRef, "beeper:synthetic-cold-account:user")
			fullEligible, err := s.ObservationFullEligibleContext(t.Context(), fullObservation.Observation.Envelope.ID)
			requirements.NoError(err)
			coldEligible, err := s.ObservationFullEligibleContext(t.Context(), coldObservation.Observation.Envelope.ID)
			requirements.NoError(err)
			assertions.True(fullEligible)
			assertions.False(coldEligible,
				"the Cold account binding must retain only its own Cold-room support")
		})
	}
}

func TestSourceMergePreservesFullProviderBindingOverColdBinding(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(4)
	ctx := t.Context()
	coldSource, err := s.GetOrCreateSource("beeper", "synthetic-cold-source")
	requirements.NoError(err)
	fullSource, err := s.GetOrCreateSource("beeper", "synthetic-full-source")
	requirements.NoError(err)
	coldRoom, err := s.EnsureConversation(coldSource.ID, "cold-room", "Cold room")
	requirements.NoError(err)
	fullRoom, err := s.EnsureConversation(fullSource.ID, "full-room", "Full room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, coldSource.ID, "cold-room", conversationpolicy.Cold))
	requirements.NoError(s.SetConversationPolicyOverride(ctx, fullSource.ID, "full-room", conversationpolicy.Full))
	participantID, err := s.EnsureParticipantByIdentifier(
		"beeper", "synthetic-source-merge-observation", "Synthetic Source Merge Observation",
	)
	requirements.NoError(err)

	coldInput := policyObservationInput(coldSource.ID, &coldRoom)
	coldInput.ProviderUserID = new("provider:cold-only")
	_, err = s.RecordContactObservationContext(ctx, participantID, coldInput)
	requirements.NoError(err)
	fullInput := policyObservationInput(fullSource.ID, &fullRoom)
	fullInput.ProviderUserID = new("provider:full")
	_, err = s.RecordContactObservationContext(ctx, participantID, fullInput)
	requirements.NoError(err)

	_, err = s.MergeSourcesContext(ctx, store.MergeSourcesRequest{
		FromSourceID: fullSource.ID,
		IntoSourceID: coldSource.ID,
	})
	requirements.NoError(err)
	current, err := s.ListParticipantObservationsContext(ctx, participantID, true)
	requirements.NoError(err)
	requirements.Len(current, 2,
		"different provider bindings must remain separate after merging source ownership")
	var fullObservation, coldObservation *store.ParticipantContactObservation
	for i := range current {
		if current[i].ProviderUserID != nil && *current[i].ProviderUserID == "provider:full" {
			fullObservation = &current[i]
		} else {
			coldObservation = &current[i]
		}
	}
	requirements.NotNil(fullObservation)
	requirements.NotNil(coldObservation)
	assertions.Nil(coldObservation.ProviderUserID,
		"source merge must clear the binding whose original support was Cold")
	fullEligible, err := s.ObservationFullEligibleContext(ctx, fullObservation.Envelope.ID)
	requirements.NoError(err)
	assertions.True(fullEligible)
	coldEligible, err := s.ObservationFullEligibleContext(ctx, coldObservation.Envelope.ID)
	requirements.NoError(err)
	assertions.False(coldEligible,
		"the Cold observation must not inherit the distinct Full provider binding's support")
}

func TestSourceMergePreservesBeeperSourceRefSupportSeparately(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(4)
	ctx := t.Context()
	coldSource, err := s.GetOrCreateSource("beeper", "synthetic-source-ref-cold-account")
	requirements.NoError(err)
	fullSource, err := s.GetOrCreateSource("beeper", "synthetic-source-ref-full-account")
	requirements.NoError(err)
	coldRoom, err := s.EnsureConversation(coldSource.ID, "cold-account-room", "Cold account room")
	requirements.NoError(err)
	fullRoom, err := s.EnsureConversation(fullSource.ID, "full-account-room", "Full account room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, coldSource.ID, "cold-account-room", conversationpolicy.Cold))
	requirements.NoError(s.SetConversationPolicyOverride(ctx, fullSource.ID, "full-account-room", conversationpolicy.Full))
	participantID, err := s.EnsureParticipantByIdentifier(
		"beeper", "synthetic-source-ref-merge-user", "Synthetic Source Ref Merge User",
	)
	requirements.NoError(err)

	coldInput := policyObservationInput(coldSource.ID, &coldRoom)
	coldInput.Envelope.SourceRef = new("beeper:synthetic-cold-account:user")
	coldInput.ProviderUserID = new("provider:shared-user")
	coldObservation, err := s.RecordContactObservationContext(ctx, participantID, coldInput)
	requirements.NoError(err)
	fullInput := policyObservationInput(fullSource.ID, &fullRoom)
	fullInput.Envelope.SourceRef = new("beeper:synthetic-full-account:user")
	fullInput.ProviderUserID = new("provider:shared-user")
	fullObservation, err := s.RecordContactObservationContext(ctx, participantID, fullInput)
	requirements.NoError(err)

	_, err = s.MergeSourcesContext(ctx, store.MergeSourcesRequest{
		FromSourceID: coldSource.ID, IntoSourceID: fullSource.ID,
	})
	requirements.NoError(err)
	current, err := s.ListParticipantObservationsContext(ctx, participantID, true)
	requirements.NoError(err)
	requirements.Len(current, 2,
		"source merge must retain separate account-bound observations for one address")
	bySourceRef := make(map[string]store.ParticipantContactObservation, len(current))
	for _, observation := range current {
		bySourceRef[*observation.Envelope.SourceRef] = observation
	}
	coldAfter, coldFound := bySourceRef["beeper:synthetic-cold-account:user"]
	fullAfter, fullFound := bySourceRef["beeper:synthetic-full-account:user"]
	requirements.True(coldFound)
	requirements.True(fullFound)
	assertions.Nil(coldAfter.ProviderUserID,
		"source merge must clear a binding whose original room is Cold")
	assertions.NotNil(fullAfter.ProviderUserID)
	assertions.Equal("provider:shared-user", *fullAfter.ProviderUserID)
	fullEligible, err := s.ObservationFullEligibleContext(ctx, fullAfter.Envelope.ID)
	requirements.NoError(err)
	assertions.True(fullEligible)
	coldEligible, err := s.ObservationFullEligibleContext(ctx, coldAfter.Envelope.ID)
	requirements.NoError(err)
	assertions.False(coldEligible,
		"the Cold account's source ref must not acquire support from the Full account")
	for observationID, roomID := range map[int64]int64{
		coldObservation.Observation.Envelope.ID: coldRoom,
		fullObservation.Observation.Envelope.ID: fullRoom,
	} {
		var supportCount int
		requirements.NoError(s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*)
			FROM participant_contact_observation_conversations WHERE observation_id=? AND conversation_id=?`),
			observationID, roomID).Scan(&supportCount))
		assertions.Equal(1, supportCount, "each observation should retain its original room support")
	}
}

func TestSourceMergeKeepsBeeperIdentifierTiedToOriginalAccount(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(4)
	ctx := t.Context()
	origin, err := s.GetOrCreateSource("beeper", "synthetic-origin-account")
	requirements.NoError(err)
	intermediate, err := s.GetOrCreateSource("beeper", "synthetic-intermediate-account")
	requirements.NoError(err)
	const roomID = "!shared-provenance:example.test"
	originRoom, err := s.EnsureConversation(origin.ID, roomID, "Shared provenance")
	requirements.NoError(err)
	intermediateRoom, err := s.EnsureConversation(intermediate.ID, roomID, "Shared provenance")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, origin.ID, roomID, conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(ctx, intermediate.ID, roomID, conversationpolicy.Full))

	const userID = "@synthetic:beeper.local"
	originIdentifier := "beeper:" + strconv.Itoa(len(origin.Identifier)) + ":" + origin.Identifier + ":" +
		strconv.Itoa(len(userID)) + ":" + userID
	participantID, err := s.EnsureParticipantByIdentifier("beeper", originIdentifier, "Synthetic User")
	requirements.NoError(err)
	sourceRef := "beeper:" + origin.Identifier + ":" + userID
	_, err = s.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		SourceID: &origin.ID, ConversationID: &originRoom,
		AddressKind: store.ContactAddressEmail, OriginalValue: "synthetic@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &sourceRef},
	})
	requirements.NoError(err)
	identifierWhere := store.FullParticipantIdentifierWhereSQLite("pi")
	if s.IsPostgreSQL() {
		identifierWhere = store.FullParticipantIdentifierWherePostgres("pi")
	}

	eligible := func() bool {
		var value bool
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT `+identifierWhere+
			` FROM participant_identifiers pi WHERE pi.participant_id=? AND pi.identifier_type='beeper' AND pi.identifier_value=?`),
			participantID, originIdentifier).Scan(&value)
		requirements.NoError(err)
		return value
	}
	assertions.True(eligible(), "the origin account identifier is supported before the source merge")
	_, err = s.MergeSourcesContext(ctx, store.MergeSourcesRequest{
		FromSourceID: origin.ID, IntoSourceID: intermediate.ID,
	})
	requirements.NoError(err)
	assertions.True(eligible(), "historical Beeper identifiers must keep using their source_ref account after merge")
	assertions.NotEqual(originRoom, intermediateRoom)

	destination, err := s.GetOrCreateSource("beeper", "synthetic-destination-account")
	requirements.NoError(err)
	destinationRoom, err := s.EnsureConversation(destination.ID, roomID, "Shared provenance")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(ctx, destination.ID, roomID, conversationpolicy.Full))
	_, err = s.MergeSourcesContext(ctx, store.MergeSourcesRequest{
		FromSourceID: intermediate.ID, IntoSourceID: destination.ID,
	})
	requirements.NoError(err)
	assertions.True(eligible(), "origin account provenance must survive sequential source merges")
	assertions.NotEqual(intermediateRoom, destinationRoom)
}

func TestSourceMergeDropsProviderBindingsWithoutCurrentFullSupport(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		originTier      conversationpolicy.Tier
		destinationTier conversationpolicy.Tier
	}{
		{name: "cold evidence cannot inherit a Full destination", originTier: conversationpolicy.Cold, destinationTier: conversationpolicy.Full},
		{name: "Full evidence loses its binding when destination is Cold", originTier: conversationpolicy.Full, destinationTier: conversationpolicy.Cold},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			s := testutil.NewTestStore(t)
			s.DB().SetMaxOpenConns(4)
			ctx := t.Context()
			origin, err := s.GetOrCreateSource("beeper", "synthetic-provider-origin-"+strconv.Itoa(len(scenario.name)))
			requirements.NoError(err)
			destination, err := s.GetOrCreateSource("beeper", "synthetic-provider-destination-"+strconv.Itoa(len(scenario.name)))
			requirements.NoError(err)
			const roomID = "!provider-merge:example.test"
			originRoom, err := s.EnsureConversation(origin.ID, roomID, "Provider merge")
			requirements.NoError(err)
			_, err = s.EnsureConversation(destination.ID, roomID, "Provider merge")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(ctx, origin.ID, roomID, scenario.originTier))
			requirements.NoError(s.SetConversationPolicyOverride(ctx, destination.ID, roomID, scenario.destinationTier))
			participantID, err := s.EnsureParticipantByIdentifier("beeper", "synthetic-provider-merge-user", "Synthetic Provider User")
			requirements.NoError(err)
			observation, err := s.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
				SourceID: &origin.ID, ConversationID: &originRoom,
				AddressKind: store.ContactAddressEmail, OriginalValue: "provider-merge@example.test",
				ProviderUserID: new("provider:origin-account"),
				Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			})
			requirements.NoError(err)
			requirements.NotNil(observation.Observation.ProviderUserID)

			_, err = s.MergeSourcesContext(ctx, store.MergeSourcesRequest{
				FromSourceID: origin.ID, IntoSourceID: destination.ID,
			})
			requirements.NoError(err)
			current, err := s.ListParticipantObservationsContext(ctx, participantID, true)
			requirements.NoError(err)
			requirements.Len(current, 1)
			assertions.Nil(current[0].ProviderUserID,
				"a provider binding must not survive without the Full-tier evidence that authorizes it")
		})
	}
}

func TestNativePhoneAndServiceIdentityRemainEligibleAfterColdChatRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, source, full, _, _ := policyObservationFixture(t)
	ctx := t.Context()
	const phone = "+15550123456"

	nativeParticipant, err := s.EnsureParticipantByPhone(phone, "Synthetic Native Contact", "whatsapp")
	requirements.NoError(err)
	nativeSource, err := s.GetOrCreateSource("whatsapp", "synthetic-native-archive")
	requirements.NoError(err)
	nativeRoom, err := s.EnsureConversation(nativeSource.ID, "native-chat", "Synthetic native chat")
	requirements.NoError(err)
	_, err = s.UpsertMessageContext(ctx, &store.Message{
		SourceID: nativeSource.ID, ConversationID: nativeRoom, SourceMessageID: "native-phone-support", MessageType: "whatsapp",
		SenderID: sql.NullInt64{Int64: nativeParticipant, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(ctx, full, nativeParticipant, "member"))

	coldOnlyParticipant, err := s.EnsureParticipantByPhone("+15550123457", "Synthetic Cold Only Contact", "whatsapp")
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipantContext(ctx, full, coldOnlyParticipant, "member"))

	phoneWhere := store.FullParticipantValueWhere("p.id", "'phone'", "p.phone_number")
	phoneCount := func(participantID int64) int64 {
		var count int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participants p WHERE p.id=? AND `+phoneWhere), participantID).Scan(&count)
		requirements.NoError(err)
		return count
	}
	identifierWhere := store.FullParticipantIdentifierWhereSQLite("pi")
	if s.IsPostgreSQL() {
		identifierWhere = store.FullParticipantIdentifierWherePostgres("pi")
	}
	identifierCount := func(participantID int64, value string) int64 {
		var count int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participant_identifiers pi
			WHERE pi.participant_id=? AND pi.identifier_type='whatsapp' AND pi.identifier_value=? AND `+identifierWhere), participantID, value).Scan(&count)
		requirements.NoError(err)
		return count
	}

	assertions.Equal(int64(1), phoneCount(nativeParticipant), "native history supports this exact phone")
	assertions.Equal(int64(1), identifierCount(nativeParticipant, phone), "native history supports this exact service identifier")
	assertions.Zero(phoneCount(coldOnlyParticipant), "room membership alone does not support a phone")
	assertions.Zero(identifierCount(coldOnlyParticipant, "+15550123457"), "room membership alone does not support a provider identifier")

	requirements.NoError(s.SetConversationPolicyOverride(ctx, source, "full", conversationpolicy.Cold))
	assertions.Equal(int64(1), phoneCount(nativeParticipant), "independent native history survives chat demotion")
	assertions.Equal(int64(1), identifierCount(nativeParticipant, phone), "independent native identity survives chat demotion")
	assertions.Zero(phoneCount(coldOnlyParticipant), "cold-only phone data stays hidden")
	assertions.Zero(identifierCount(coldOnlyParticipant, "+15550123457"), "cold-only provider identifier stays hidden")
}

func TestImportedNativePhoneIdentifiersRemainEligibleAfterColdChatRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, chatSource, full, _, _ := policyObservationFixture(t)
	ctx := t.Context()
	type nativePhone struct {
		name           string
		sourceType     string
		identifierType string
		messageType    string
		phone          string
		participantID  int64
	}
	fixtures := []nativePhone{
		{name: "imazing-csv", sourceType: "imazing_csv", identifierType: "imazing_csv", messageType: "sms", phone: "+12025550158"},
		{name: "synctech-sms", sourceType: "synctech_sms", identifierType: "synctech_sms", messageType: "sms", phone: "+12025550159"},
		{name: "apple-messages", sourceType: "apple_messages", identifierType: "imessage", messageType: "imessage", phone: "+12025550160"},
	}
	for i := range fixtures {
		fixture := &fixtures[i]
		participantID, err := s.EnsureParticipantByPhone(fixture.phone, "Synthetic "+fixture.name, fixture.identifierType)
		requirements.NoError(err)
		fixture.participantID = participantID
		source, err := s.GetOrCreateSource(fixture.sourceType, "native-"+fixture.name)
		requirements.NoError(err)
		room, err := s.EnsureConversation(source.ID, "native-room", "Native history")
		requirements.NoError(err)
		_, err = s.UpsertMessageContext(ctx, &store.Message{
			SourceID: source.ID, ConversationID: room, SourceMessageID: "native-phone-support",
			MessageType: fixture.messageType, SenderID: sql.NullInt64{Int64: participantID, Valid: true},
		})
		requirements.NoError(err)
		requirements.NoError(s.EnsureConversationParticipantContext(ctx, full, participantID, "member"))
	}

	phoneWhere := store.FullParticipantValueWhere("p.id", "'phone'", "p.phone_number")
	identifierWhere := store.FullParticipantIdentifierWhereSQLite("pi")
	if s.IsPostgreSQL() {
		identifierWhere = store.FullParticipantIdentifierWherePostgres("pi")
	}
	for _, fixture := range fixtures {
		var phoneCount, identifierCount int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participants p WHERE p.id=? AND `+phoneWhere), fixture.participantID).Scan(&phoneCount)
		requirements.NoError(err)
		err = s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participant_identifiers pi
			WHERE pi.participant_id=? AND pi.identifier_type=? AND pi.identifier_value=? AND `+identifierWhere), fixture.participantID, fixture.identifierType, fixture.phone).Scan(&identifierCount)
		requirements.NoError(err)
		assertions.Equal(int64(1), phoneCount, fixture.name+" native history supports its exact phone")
		assertions.Equal(int64(1), identifierCount, fixture.name+" native history supports its exact identifier")
	}

	requirements.NoError(s.SetConversationPolicyOverride(ctx, chatSource, "full", conversationpolicy.Cold))
	for _, fixture := range fixtures {
		var phoneCount, identifierCount int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participants p WHERE p.id=? AND `+phoneWhere), fixture.participantID).Scan(&phoneCount)
		requirements.NoError(err)
		err = s.DB().QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM participant_identifiers pi
			WHERE pi.participant_id=? AND pi.identifier_type=? AND pi.identifier_value=? AND `+identifierWhere), fixture.participantID, fixture.identifierType, fixture.phone).Scan(&identifierCount)
		requirements.NoError(err)
		assertions.Equal(int64(1), phoneCount, fixture.name+" native phone remains supported after chat demotion")
		assertions.Equal(int64(1), identifierCount, fixture.name+" native identifier remains supported after chat demotion")
	}
}

func policyObservationSupport(t *testing.T, s *store.Store, id int64) []int64 {
	t.Helper()
	rows, err := s.DB().Query(s.Rebind(`SELECT conversation_id FROM participant_contact_observation_conversations WHERE observation_id=? ORDER BY conversation_id`), id)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestConversationPolicyObservationMixedSupport(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	input := policyObservationInput(source, &cold)
	first, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	found, err := s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Empty(found)
	peer, err := s.EnsureParticipantByIdentifier("test", "independent-peer", "")
	requirements.NoError(err)
	independent := policyObservationInput(source, nil)
	independent.SourceID = nil
	independent.ProviderUserID = new("another-provider")
	_, err = s.RecordContactObservationContext(t.Context(), peer, independent)
	requirements.NoError(err)
	candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assertions.Empty(candidates)
	input.ConversationID = &full
	second, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	assertions.Equal(first.Observation.Envelope.ID, second.Observation.Envelope.ID)
	found, err = s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Len(found, 1)
	candidates, err = s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assertions.Len(candidates, 1)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	candidates, err = s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assertions.Empty(candidates)
	raw, err := s.ListParticipantObservationsContext(t.Context(), pid, true)
	requirements.NoError(err)
	assertions.Len(raw, 1)
}

func TestConversationPolicyObservationDedupKeepsEveryRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	input := policyObservationInput(source, &cold)
	first, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	before, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	input.ConversationID = &full
	second, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	assertions.False(second.Created)
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, s, first.Observation.Envelope.ID))
	after, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before)
	_, err = s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	repeated, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(after, repeated)
	other, err := s.GetOrCreateSource("beeper", "other-account")
	requirements.NoError(err)
	wrong, err := s.EnsureConversation(other.ID, "wrong", "Wrong")
	requirements.NoError(err)
	input.ConversationID = &wrong
	_, err = s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.Error(err)
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, s, first.Observation.Envelope.ID))
}

func TestConversationPolicyLegacyObservationCannotBorrowFullSourceRoom(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, _, _, pid := policyObservationFixture(t)
	input := policyObservationInput(source, nil)
	_, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	found, err := s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Empty(found)
	input.SourceID = nil
	_, err = s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	found, err = s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
	requirements.NoError(err)
	assertions.Len(found, 1)
}

func TestConversationPolicyDemotionPreservesManualIdentityDecision(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "accepted"}[accept], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s, source, full, _, pid := policyObservationFixture(t)
			peer, err := s.EnsureParticipantByIdentifier("test", "manual-peer", "")
			requirements.NoError(err)
			input := policyObservationInput(source, &full)
			_, err = s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchStableProviderID, NormalizedValue: input.ProviderUserID, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source})
			requirements.NoError(err)
			if accept {
				_, _, err = s.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "user", nil)
			} else {
				_, err = s.DecideIdentityMatchCandidateContext(t.Context(), candidate.ID, store.IdentityMatchStateRejected, "user", nil)
			}
			requirements.NoError(err)
			person, _, err := s.CreatePersonFromParticipant(pid)
			requirements.NoError(err)
			contactBefore, err := s.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "curated@example.org", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			requirements.NoError(err)
			before, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
			after, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			requirements.NoError(err)
			assertions.Equal(before, after)
			contacts, err := s.ListPersonContactPointsContext(t.Context(), person.ID, true)
			requirements.NoError(err)
			requirements.Len(contacts, 1)
			assertions.Equal(*contactBefore, contacts[0])
		})
	}
}

func TestConversationPolicyObservationMergeSubsetRoundTrip(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	// Subset is a SQLite export API; exercise its real file-backed path.
	path := filepath.Join(t.TempDir(), "source.db")
	s, err := store.Open(path)
	requirements.NoError(err)
	defer func() { require.NoError(t, s.Close()) }()
	requirements.NoError(s.InitSchema())
	source, err := s.GetOrCreateSource("beeper", "merge-account")
	requirements.NoError(err)
	full, err := s.EnsureConversation(source.ID, "full", "Full")
	requirements.NoError(err)
	cold, err := s.EnsureConversation(source.ID, "cold", "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "full", conversationpolicy.Full))
	left, err := s.EnsureParticipantByIdentifier("test", "merge-left", "")
	requirements.NoError(err)
	right, err := s.EnsureParticipantByIdentifier("test", "merge-right", "")
	requirements.NoError(err)
	survivor, err := s.RecordContactObservationContext(t.Context(), left, policyObservationInput(source.ID, &cold))
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), right, policyObservationInput(source.ID, &full))
	requirements.NoError(err)
	requirements.NoError(s.MergeParticipants(right, left))
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, s, survivor.Observation.Envelope.ID))
	for _, room := range []int64{full, cold} {
		_, err = s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: map[int64]string{full: "full-message", cold: "cold-message"}[room], MessageType: "beeper", SenderID: sql.NullInt64{Int64: left, Valid: true}})
		requirements.NoError(err)
	}
	dst := filepath.Join(t.TempDir(), "subset")
	_, err = store.CopySubsetWithOptions(path, dst, 10, store.CopySubsetOptions{IncludeProfiles: true})
	requirements.NoError(err)
	copied, err := store.Open(filepath.Join(dst, "msgvault.db"))
	requirements.NoError(err)
	defer func() { require.NoError(t, copied.Close()) }()
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, copied, survivor.Observation.Envelope.ID))
	requirements.NoError(copied.RemoveSource(source.ID))
	assertions.Empty(policyObservationSupport(t, copied, survivor.Observation.Envelope.ID))
}

func TestConversationPolicyObservationSourceMerge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	// Existing source maintenance reserves sync-lease connections separately.
	s.DB().SetMaxOpenConns(4)
	other, err := s.GetOrCreateSource("beeper", "merge-destination")
	requirements.NoError(err)
	destination, err := s.EnsureConversation(other.ID, "destination", "Destination")
	requirements.NoError(err)
	kept, err := s.RecordContactObservationContext(t.Context(), pid, policyObservationInput(other.ID, &destination))
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), pid, policyObservationInput(source, &full))
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), pid, policyObservationInput(source, &cold))
	requirements.NoError(err)
	_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source, IntoSourceID: other.ID})
	requirements.NoError(err)
	current, err := s.ListParticipantObservationsContext(t.Context(), pid, true)
	requirements.NoError(err)
	requirements.Len(current, 2, "the Cold destination binding must stay separate from Full-backed provider evidence")
	var fullBinding *store.ParticipantContactObservation
	var destinationBinding *store.ParticipantContactObservation
	for i := range current {
		observation := &current[i]
		if observation.ProviderUserID == nil {
			destinationBinding = observation
		} else {
			fullBinding = observation
		}
	}
	requirements.NotNil(fullBinding)
	requirements.NotNil(destinationBinding)
	assertions.Equal("provider:synthetic-user", *fullBinding.ProviderUserID)
	assertions.Equal([]int64{full, cold}, policyObservationSupport(t, s, fullBinding.Envelope.ID))
	assertions.Equal([]int64{destination}, policyObservationSupport(t, s, destinationBinding.Envelope.ID))
	assertions.Equal(kept.Observation.Envelope.ID, destinationBinding.Envelope.ID)
	found, err := s.FindObservationsByProviderUserIDContext(t.Context(), "provider:synthetic-user", 0)
	requirements.NoError(err)
	assertions.Len(found, 1)
}

func TestConversationPolicyObservationSourceMergePreservesScopedBindings(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, from, _, _, participantID := policyObservationFixture(t)
	s.DB().SetMaxOpenConns(4)
	into, err := s.GetOrCreateSource("beeper", "merge-scoped-destination")
	requirements.NoError(err)
	fromRoom, err := s.EnsureConversation(from, "scoped-source-room", "Scoped source room")
	requirements.NoError(err)
	intoRoom, err := s.EnsureConversation(into.ID, "scoped-destination-room", "Scoped destination room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), from, "scoped-source-room", conversationpolicy.Full))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), into.ID, "scoped-destination-room", conversationpolicy.Full))

	for _, binding := range []struct {
		providerID string
		sourceRef  string
		room       int64
		source     int64
	}{
		{providerID: "provider:scoped-left", sourceRef: "beeper:scoped-account:left", room: fromRoom, source: from},
		{providerID: "provider:scoped-right", sourceRef: "beeper:scoped-account:right", room: intoRoom, source: into.ID},
	} {
		input := store.ParticipantContactObservationInput{
			SourceID: &binding.source, ConversationID: &binding.room,
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
			ScopeKind: new("workspace"), ScopeValue: new("T0SYNTHETIC"),
			ProviderUserID: &binding.providerID, OriginalValue: "shared-user",
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceArchiveObservation, SourceRef: &binding.sourceRef,
			},
		}
		_, err = s.RecordContactObservationContext(t.Context(), participantID, input)
		requirements.NoError(err)
	}

	_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
	requirements.NoError(err)
	current, err := s.ListParticipantObservationsContext(t.Context(), participantID, true)
	requirements.NoError(err)
	requirements.Len(current, 2,
		"the current identity index must allow separately scoped provider bindings after source merge")
	providers := make([]string, 0, len(current))
	for _, observation := range current {
		requirements.NotNil(observation.ProviderUserID)
		providers = append(providers, *observation.ProviderUserID)
		requirements.NotNil(observation.SourceID)
		assertions.Equal(into.ID, *observation.SourceID)
	}
	assertions.ElementsMatch([]string{"provider:scoped-left", "provider:scoped-right"}, providers)
}

func TestConversationPolicyObservationScoringRequiresBothFullSides(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-%t", legacy), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s, source, full, cold, pid := policyObservationFixture(t)
			other, err := s.GetOrCreateSource("gmail", "independent@example.org")
			requirements.NoError(err)
			peer, err := s.EnsureParticipantByIdentifier("test", "score-peer", "")
			requirements.NoError(err)
			first := policyObservationInput(source, &cold)
			second := policyObservationInput(other.ID, nil)
			_, err = s.RecordContactObservationContext(t.Context(), pid, first)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, second)
			requirements.NoError(err)
			count, err := s.EnsurePersonMatchScoringCandidatesContext(t.Context(), 10)
			requirements.NoError(err)
			assertions.Zero(count)
			first.ConversationID = &full
			_, err = s.RecordContactObservationContext(t.Context(), pid, first)
			requirements.NoError(err)
			count, err = s.EnsurePersonMatchScoringCandidatesContext(t.Context(), 10)
			requirements.NoError(err)
			assertions.Equal(1, count)
			candidatesBefore, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
			requirements.NoError(err)
			requirements.Len(candidatesBefore, 1)
			// A source attachment without independent producer evidence corroborates
			// this pair; it cannot turn one surviving endpoint into an independent claim.
			contacts, err := s.GetOrCreateSource("muesli", "untyped-corroboration")
			requirements.NoError(err)
			requirements.NoError(s.AttachIdentityMatchCandidateSourceContext(t.Context(), candidatesBefore[0].ID, contacts.ID))
			if legacy {
				_, err = s.DB().Exec(s.Rebind(`UPDATE identity_match_candidates SET source_ref=NULL WHERE id=?`), candidatesBefore[0].ID)
				requirements.NoError(err)
				_, err = s.DB().Exec(s.Rebind(`UPDATE identity_match_evidence SET evidence_ref=NULL WHERE candidate_id=?`), candidatesBefore[0].ID)
				requirements.NoError(err)
			}

			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
			candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
			requirements.NoError(err)
			assertions.Empty(candidates)
			independent, err := s.ListParticipantObservationsContext(t.Context(), peer, true)
			requirements.NoError(err)
			assertions.Len(independent, 1)
		})
	}
}

func TestConversationPolicyObservationStaleMatcherCannotRecreateCandidate(t *testing.T) {
	requirements := require.New(t)

	s, source, full, _, pid := policyObservationFixture(t)
	peer, err := s.EnsureParticipantByIdentifier("test", "stale-peer", "")
	requirements.NoError(err)
	input := policyObservationInput(source, &full)
	observation, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	candidateInput := store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchStableProviderID, NormalizedValue: input.ProviderUserID, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source, ObservationID: observation.Observation.Envelope.ID}
	_, _, err = s.UpsertIdentityMatchCandidateContext(t.Context(), candidateInput)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	_, _, err = s.UpsertIdentityMatchCandidateContext(t.Context(), candidateInput)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 20, 0)
	requirements.NoError(err)
	assert.Empty(t, candidates)
}

func TestConversationPolicyObservationStaleSystemAcceptance(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "accept", true: "resume"}[resume], func(t *testing.T) {
			requirements := require.New(t)

			s, source, _, cold, pid := policyObservationFixture(t)
			s.DB().SetMaxOpenConns(1)
			peer, err := s.EnsureParticipantByIdentifier("test", "late-peer", "")
			requirements.NoError(err)
			input := policyObservationInput(source, &cold)
			_, err = s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, input)
			requirements.NoError(err)
			// A candidate read before demotion, or an old pending system decision,
			// cannot use cold support when its actual publication transaction begins.
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchStableProviderID, NormalizedValue: input.ProviderUserID, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source})
			requirements.NoError(err)
			if resume {
				_, err = s.DB().Exec(s.Rebind(`UPDATE identity_match_candidates SET state='accepted',decided_by='system',application_pending=TRUE WHERE id=?`), candidate.ID)
				requirements.NoError(err)
				_, _, _, err = s.ResumeAcceptedIdentityMatchCandidateContext(t.Context(), candidate.ID)
			} else {
				_, _, err = s.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
			}
			requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			members, err := s.ClusterMembers(pid)
			requirements.NoError(err)
			assert.NotContains(t, members, peer)
		})
	}
}

func TestConversationPolicyLegacyDerivedObservationRequiresRoomSupport(t *testing.T) {
	for _, provenance := range []store.Provenance{store.ProvenanceExtraction, store.ProvenanceEnrichment, store.ProvenanceSystem} {
		t.Run(string(provenance), func(t *testing.T) {
			s, source, _, _, pid := policyObservationFixture(t)
			input := policyObservationInput(source, nil)
			input.Envelope.Source = provenance
			_, err := s.RecordContactObservationContext(t.Context(), pid, input)
			require.NoError(t, err)
			found, err := s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
			require.NoError(t, err)
			assert.Empty(t, found)
		})
	}
}

// seedFullObservationTestSupport gives legacy matching fixtures explicit full
// conversation provenance. Production never infers these support edges.
func seedFullObservationTestSupport(t *testing.T, s *store.Store) {
	t.Helper()
	rows, err := s.DB().Query(`SELECT id,source_id FROM participant_contact_observations WHERE source_id IS NOT NULL`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	type support struct{ observation, source int64 }
	var pending []support
	for rows.Next() {
		var item support
		require.NoError(t, rows.Scan(&item.observation, &item.source))
		pending = append(pending, item)
	}
	require.NoError(t, rows.Err())
	rooms := map[int64]int64{}
	for _, item := range pending {
		room, ok := rooms[item.source]
		if !ok {
			room = fullObservationTestRoom(t, s, item.source)
			rooms[item.source] = room
		}
		_, err = s.DB().Exec(s.Rebind(`INSERT INTO participant_contact_observation_conversations(observation_id,conversation_id) VALUES(?,?) ON CONFLICT(observation_id,conversation_id) DO NOTHING`), item.observation, room)
		require.NoError(t, err)
	}
}

func fullObservationTestRoom(t *testing.T, s *store.Store, source int64) int64 {
	t.Helper()
	room, err := s.EnsureConversation(source, "identity-test-room", "Identity fixture")
	require.NoError(t, err)
	var kind string
	require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT source_type FROM sources WHERE id=?`), source).Scan(&kind))
	if conversationpolicy.Supported(kind) {
		require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source, "identity-test-room", conversationpolicy.Full))
	}
	return room
}

func TestConversationPolicyObservationCrossScopeRequiresEligiblePeerScope(t *testing.T) {
	requirements := require.New(t)

	s, source, full, cold, pid := policyObservationFixture(t)
	peer, err := s.EnsureParticipantByIdentifier("test", "scope-peer", "")
	requirements.NoError(err)
	input := store.ParticipantContactObservationInput{SourceID: &source, ConversationID: &full, AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"), ScopeKind: new("workspace"), ScopeValue: new("full-scope"), OriginalValue: "scope-user", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
	origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	input.ConversationID = &cold
	input.ScopeValue = new("cold-scope")
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	_, _, err = s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{ObservationID: origin.Observation.Envelope.ID, LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchServiceScopeUsername, ServiceSlug: input.ServiceSlug, CrossScope: true, NormalizedValue: new("scope-user"), State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &source})
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
}

func TestConversationPolicyObservationLateEvidenceCannotSurviveDemotion(t *testing.T) {
	requirements := require.New(t)

	s, source, full, _, pid := policyObservationFixture(t)
	peer, err := s.EnsureParticipantByIdentifier("test", "late-evidence-peer", "")
	requirements.NoError(err)
	input := policyObservationInput(source, &full)
	origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), peer, input)
	requirements.NoError(err)
	candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
	requirements.NoError(err)
	revision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	gathered, err := s.ListFullParticipantObservationsContext(t.Context(), pid)
	requirements.NoError(err)
	requirements.Len(gathered, 1)
	evidence := store.IdentityMatchEvidenceInput{EvidenceKind: "email", Detail: new(gathered[0].NormalizedValue), Source: store.ProvenanceArchiveObservation, SourceID: &source, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin.Observation.Envelope.ID}}
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
	_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	kept, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
	requirements.NoError(err)
	assert.Empty(t, kept.Evidence)
	// Even a fresh revision cannot substitute for the origin's live support.
	evidence.Admission.PolicyRevision, err = s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "manual", Detail: new("user-reviewed"), Source: store.ProvenanceUser})
	requirements.NoError(err)
}

func TestConversationPolicyNameEvidenceRequiresExactFullNames(t *testing.T) {
	for _, tc := range []struct {
		name                string
		leftName, rightName string
		rightRaw            string
		want                bool
	}{
		{name: "blank", rightRaw: "Synthetic User"},
		{name: "different", leftName: "Different User", rightName: "Different User", rightRaw: "Synthetic User"},
		{name: "one-endpoint", leftName: "Synthetic User", rightName: "Different User", rightRaw: "Synthetic User"},
		{name: "exact", leftName: "Synthetic User", rightName: "Synthetic User", rightRaw: "Synthetic User", want: true},
		{name: "case-folded-pair", leftName: "Synthetic User", rightName: "SYNTHETIC USER", rightRaw: "SYNTHETIC USER", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)

			s, source, full, cold, pid := policyObservationFixture(t)
			peer, err := s.EnsureParticipantByIdentifier("beeper", "name-peer", tc.rightRaw)
			requirements.NoError(err)
			policyNameEnvelope(t, s, source, cold, "cold-name", []int64{pid, peer}, []string{"Synthetic User", "Synthetic User"})
			policyNameEnvelope(t, s, source, full, "full-name", []int64{pid, peer}, []string{tc.leftName, tc.rightName})
			input := policyObservationInput(source, &full)
			origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, input)
			requirements.NoError(err)
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
			requirements.NoError(err)
			revision, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			evidence := store.IdentityMatchEvidenceInput{EvidenceKind: "display_name", Detail: new("Synthetic User"), Source: store.ProvenanceArchiveObservation, SourceID: &source, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin.Observation.Envelope.ID}}
			_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
			if tc.want {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			}
			evidence.EvidenceKind, evidence.Detail = "email", new("shared@example.org")
			_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
			requirements.NoError(err, "independent Full address evidence must survive")
			evidence.EvidenceKind, evidence.Detail, evidence.Source, evidence.Admission = "display_name", new("Synthetic User"), store.ProvenanceUser, nil
			_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
			requirements.NoError(err, "manual name evidence remains independent")
		})
	}
}

func TestConversationPolicyObservationDemotionRetractsGeneratedExplanations(t *testing.T) {
	for _, support := range []string{"none", "blank-name", "different-name", "exact-name"} {
		t.Run("remaining-full-"+support, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s, source, full, alternate, pid := policyObservationFixture(t)
			peer, err := s.EnsureParticipantByIdentifier("test", "explanation-peer", "Synthetic User")
			requirements.NoError(err)
			policyNameEnvelope(t, s, source, full, "original-name", []int64{pid, peer}, []string{"Synthetic User", "Synthetic User"})
			input := policyObservationInput(source, &full)
			origin, err := s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			_, err = s.RecordContactObservationContext(t.Context(), peer, input)
			requirements.NoError(err)
			if support != "none" {
				requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
				name := map[string]string{"blank-name": "", "different-name": "Different User", "exact-name": "Synthetic User"}[support]
				policyNameEnvelope(t, s, source, alternate, "alternate-name", []int64{pid, peer}, []string{name, name})
				input.ConversationID = &alternate
				_, err = s.RecordContactObservationContext(t.Context(), pid, input)
				requirements.NoError(err)
				_, err = s.RecordContactObservationContext(t.Context(), peer, input)
				requirements.NoError(err)
			}
			requirements.NoError(s.EnsureConversationParticipant(full, pid, "member"))
			requirements.NoError(s.EnsureConversationParticipant(full, peer, "member"))
			candidate, _, err := s.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: pid, RightKind: store.IdentityMatchParticipant, RightID: peer, Basis: store.IdentityMatchDisplayName, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
			requirements.NoError(err)
			revision, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			for _, item := range []struct{ kind, detail, ref string }{{"email", "shared@example.org", ""}, {"display_name", "Synthetic User", ""}, {"conversation_membership", "Shared conversation", fmt.Sprintf("conversation:%d", full)}} {
				evidence := store.IdentityMatchEvidenceInput{EvidenceKind: item.kind, Detail: &item.detail, Source: store.ProvenanceArchiveObservation, SourceID: &source, Admission: &store.IdentityMatchEvidenceAdmission{PolicyRevision: revision, ObservationID: origin.Observation.Envelope.ID}}
				if item.ref != "" {
					evidence.EvidenceRef = &item.ref
				}
				_, err = s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, evidence)
				requirements.NoError(err)
			}
			manual, err := s.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "display_name", Detail: new("Synthetic User"), Source: store.ProvenanceUser})
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "full", conversationpolicy.Cold))
			kept, err := s.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			requirements.NoError(err)
			automatic := map[string]bool{}
			manualKept := false
			for _, evidence := range kept.Evidence {
				if evidence.ID == manual.ID {
					manualKept = true
				} else {
					automatic[evidence.EvidenceKind] = true
				}
			}
			wantEvidence := 1 // The independent manual name evidence.
			if support != "none" {
				wantEvidence++ // The independently supported Full email evidence.
			}
			if support == "exact-name" {
				wantEvidence++
			}
			assertions.Len(kept.Evidence, wantEvidence)
			assertions.True(manualKept)
			assertions.Equal(support != "none", automatic["email"])
			assertions.Equal(support == "exact-name", automatic["display_name"])
			assertions.False(automatic["conversation_membership"])
			assertions.Equal(store.IdentityMatchStateCandidate, kept.State, "manual candidate decision survives")
		})
	}
}

func TestConversationPolicyPromotionGeneratesColdScopedAddressConflict(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, source, _, cold, left := policyObservationFixture(t)
	right, err := s.EnsureParticipantByIdentifier("test", "promoted-conflict-peer", "")
	requirements.NoError(err)
	input := store.ParticipantContactObservationInput{SourceID: &source, ConversationID: &cold,
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
		ScopeKind: new("workspace"), ScopeValue: new("T0EXAMPLE"), OriginalValue: "shared-user",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
	for _, id := range []int64{left, right} {
		_, err = s.RecordContactObservationContext(t.Context(), id, input)
		requirements.NoError(err)
	}
	candidates, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	requirements.Empty(candidates)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "cold", conversationpolicy.Full))
	for _, id := range []int64{left, right} {
		_, err = s.RecordContactObservationContext(t.Context(), id, input)
		requirements.NoError(err)
	}
	candidates, err = s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.Equal(store.IdentityMatchStateConflict, candidates[0].State)
	assertions.Equal(store.IdentityMatchServiceScopeUsername, candidates[0].Basis)
	before, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	identityBefore, err := s.IdentityRevisionContext(t.Context())
	requirements.NoError(err)
	for _, id := range []int64{left, right} {
		repeated, err := s.RecordContactObservationContext(t.Context(), id, input)
		requirements.NoError(err)
		assertions.False(repeated.Created)
	}
	repeated, err := s.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	assertions.Equal(candidates, repeated)
	after, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, after)
	identityAfter, err := s.IdentityRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(identityBefore, identityAfter)
}

// Clearing a Cold provider binding before a source merge can make it match a
// providerless Full observation of the same scoped address in that source.
func TestSourceMergeClearsColdProviderBindingBesideProviderlessFullObservation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s, from, full, cold, participantID := policyObservationFixture(t)
	s.DB().SetMaxOpenConns(4)
	into, err := s.GetOrCreateSource("beeper", "merge-collision-destination")
	requirements.NoError(err)
	input := func(room int64, providerID *string) store.ParticipantContactObservationInput {
		return store.ParticipantContactObservationInput{
			SourceID: &from, ConversationID: &room,
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
			ScopeKind: new("workspace"), ScopeValue: new("T0SYNTHETIC"),
			ProviderUserID: providerID, OriginalValue: "shared-user",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		}
	}
	_, err = s.RecordContactObservationContext(t.Context(), participantID, input(full, nil))
	requirements.NoError(err)
	_, err = s.RecordContactObservationContext(t.Context(), participantID, input(cold, new("provider:cold-only")))
	requirements.NoError(err)
	before, err := s.ListParticipantObservationsContext(t.Context(), participantID, true)
	requirements.NoError(err)
	requirements.Len(before, 2, "the Cold binding is a separate current observation")

	_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
	requirements.NoError(err)
	current, err := s.ListParticipantObservationsContext(t.Context(), participantID, true)
	requirements.NoError(err)
	requirements.Len(current, 1)
	assertions.Nil(current[0].ProviderUserID)
	var rooms []int64
	rows, err := s.DB().QueryContext(t.Context(), s.Rebind(`SELECT conversation_id FROM participant_contact_observation_conversations WHERE observation_id=? ORDER BY conversation_id`), current[0].Envelope.ID)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var room int64
		requirements.NoError(rows.Scan(&room))
		rooms = append(rooms, room)
	}
	requirements.NoError(rows.Err())
	assertions.ElementsMatch([]int64{full, cold}, rooms, "the kept observation keeps support from both rooms")
	all, err := s.ListParticipantObservationsContext(t.Context(), participantID, false)
	requirements.NoError(err)
	assertions.Len(all, 2, "the Cold observation stays as history")
}
