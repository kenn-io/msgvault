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
