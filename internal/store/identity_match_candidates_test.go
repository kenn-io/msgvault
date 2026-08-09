package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestUsernameOnlyCandidateCannotBeAcceptedWithoutCorroboration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@bob:example.org", "Bob Example")
	require.NoError(err)
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchServiceScopeUsername, ServiceSlug: new("x"),
		NormalizedValue: new("shared"), State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	assert.True(created)
	for _, kind := range []string{"phone", "display_name"} {
		_, err := st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: kind, Source: store.ProvenanceArchiveObservation,
		})
		require.NoError(err)
	}
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil,
	)
	require.ErrorIs(err, store.ErrIdentityMatchNotAcceptable)
	accepted, err := st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "user", new("confirmed"),
	)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
	assert.Len(accepted.Evidence, 2)
}

func TestStableProviderIDCandidateMayBeAcceptedBySystem(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@alice2:example.org", "Alice Example")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(context.Background(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchStableProviderID, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	accepted, err := st.DecideIdentityMatchCandidateContext(
		context.Background(), candidate.ID, store.IdentityMatchStateAccepted, "system", nil,
	)
	require.NoError(err)
	assert.NotNil(accepted.DecidedAt)
}

func TestUpsertIdentityMatchCandidateRejectsDecisionStates(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("example", "left", "Left User")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right", "Right User")
	require.NoError(err)

	for _, state := range []store.IdentityMatchState{
		store.IdentityMatchStateAccepted,
		store.IdentityMatchStateRejected,
	} {
		_, created, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchStableProviderID, State: state,
			Source: store.ProvenanceArchiveObservation,
		})
		require.ErrorIs(err, store.ErrInvalidIdentityMatchState, state)
		require.False(created, state)
	}

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Empty(candidates, "decision states must not create candidates directly")
}

func TestRejectedCandidateIsRetainedAndEndpointsCanonical(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@bob:example.org", "Bob Example")
	require.NoError(err)
	input := store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	}
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, input)
	require.NoError(err)
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil,
	)
	require.NoError(err)
	input.LeftID, input.RightID = right, left
	again, created, err := st.UpsertIdentityMatchCandidateContext(ctx, input)
	require.NoError(err)
	assert.False(created)
	assert.Equal(store.IdentityMatchStateRejected, again.State)
	_, _, err = st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: left,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.ErrorIs(err, store.ErrIdentityMatchSelfLink)
}

func TestObservationConflictPromotesNeutralCandidateAndPreservesReviewProvenance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "left-promotion", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right-promotion", "Right")
	require.NoError(err)
	normalized := "shared@example.org"
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceSystem,
		},
	)
	require.NoError(err)
	require.True(created)
	note := "keep this review note"
	candidate, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateCandidate, "user", &note,
	)
	require.NoError(err)
	require.NotNil(candidate.DecidedAt)

	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: normalized,
		ProviderUserID: new("provider-left"),
		Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	_, err = st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ProviderUserID = new("provider-right")
	result, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.True(result.Conflicting)
	require.NotNil(result.CandidateID)
	assert.Equal(candidate.ID, *result.CandidateID)

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	promoted := candidates[0]
	assert.Equal(candidate.ID, promoted.ID)
	assert.Equal(store.IdentityMatchStateConflict, promoted.State)
	assert.Equal(store.ProvenanceSystem, promoted.Source)
	assert.Equal(candidate.DecidedBy, promoted.DecidedBy)
	assert.Equal(candidate.DecidedAt, promoted.DecidedAt)
	assert.Equal(candidate.Notes, promoted.Notes)
}

func TestIdentityMatchCandidatesKeepDistinctNormalizedValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "left-value", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right-value", "Right")
	require.NoError(err)

	var ids []int64
	for _, value := range []string{"first@example.org", "second@example.org"} {
		candidate, created, err := st.UpsertIdentityMatchCandidateContext(
			ctx, store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: left,
				RightKind: store.IdentityMatchParticipant, RightID: right,
				Basis: store.IdentityMatchEmail, NormalizedValue: &value,
				State:  store.IdentityMatchStateCandidate,
				Source: store.ProvenanceArchiveObservation,
			},
		)
		require.NoError(err)
		assert.True(created, value)
		ids = append(ids, candidate.ID)
	}

	assert.NotEqual(ids[0], ids[1])
	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.ElementsMatch(
		[]string{"first@example.org", "second@example.org"},
		[]string{*candidates[0].NormalizedValue, *candidates[1].NormalizedValue},
	)
}

func TestIdentityMatchCandidateRequiresExistingEndpoints(t *testing.T) {
	requirements := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@candidate-owner:example.org", "Candidate Owner",
	)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)
	point, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "owner@example.org",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	observation, err := st.RecordContactObservationContext(
		ctx, participantID, store.ParticipantContactObservationInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "observed@example.org",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		},
	)
	requirements.NoError(err)

	for _, test := range []struct {
		name string
		kind store.IdentityMatchEndpointKind
		id   int64
	}{
		{name: "participant", kind: store.IdentityMatchParticipant, id: participantID},
		{name: "person", kind: store.IdentityMatchPerson, id: person.ID},
		{name: "observation", kind: store.IdentityMatchObservation,
			id: observation.Observation.Envelope.ID},
		{name: "contact point", kind: store.IdentityMatchContactPoint,
			id: point.Envelope.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			_, created, err := st.UpsertIdentityMatchCandidateContext(
				ctx, store.IdentityMatchCandidateInput{
					LeftKind: test.kind, LeftID: test.id + 1_000_000,
					RightKind: store.IdentityMatchParticipant, RightID: participantID,
					Basis:  store.IdentityMatchDisplayName,
					State:  store.IdentityMatchStateCandidate,
					Source: store.ProvenanceSystem,
				},
			)
			require.ErrorIs(err, store.ErrIdentityMatchEndpointNotFound)
			require.False(created)
		})
	}
}

func TestDeletePersonRemovesCandidatesForDeletedProfileEndpoints(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	owner, err := st.EnsureParticipantByIdentifier("example", "owner", "Owner")
	require.NoError(err)
	other, err := st.EnsureParticipantByIdentifier("example", "other", "Other")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(owner)
	require.NoError(err)
	point, err := st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "owner@example.org",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	for _, endpoint := range []struct {
		kind store.IdentityMatchEndpointKind
		id   int64
	}{
		{kind: store.IdentityMatchPerson, id: person.ID},
		{kind: store.IdentityMatchContactPoint, id: point.Envelope.ID},
	} {
		_, _, err = st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
			LeftKind: endpoint.kind, LeftID: endpoint.id,
			RightKind: store.IdentityMatchParticipant, RightID: other,
			Basis:  store.IdentityMatchDisplayName,
			State:  store.IdentityMatchStateCandidate,
			Source: store.ProvenanceSystem,
		})
		require.NoError(err)
	}

	person, err = st.GetPersonContext(ctx, person.ID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(ctx, person.ID, person.Revision))
	var remaining int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM identity_match_candidates`).Scan(&remaining))
	assert.Zero(remaining, "deleted person and contact-point endpoints must not dangle")
}
