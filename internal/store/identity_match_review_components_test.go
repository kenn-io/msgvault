package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIdentityMatchReviewRejectsExpandedUnboundComponent(t *testing.T) {
	for _, stage := range []string{"before_acceptance", "during_application", "recovery"} {
		for _, endpoint := range []string{"left", "right"} {
			t.Run(stage+"/"+endpoint, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st := storetest.New(t).Store
				ctx := t.Context()
				left, err := st.EnsureParticipantByIdentifier("beeper", "component-left", "Synthetic Left")
				require.NoError(err)
				right, err := st.EnsureParticipantByIdentifier("beeper", "component-right", "Synthetic Right")
				require.NoError(err)
				extra, err := st.EnsureParticipantByIdentifier("beeper", "component-extra", "Synthetic Extra")
				require.NoError(err)
				candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
					LeftKind: store.IdentityMatchParticipant, LeftID: left,
					RightKind: store.IdentityMatchParticipant, RightID: right,
					Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
					Source: store.ProvenanceArchiveObservation,
				})
				require.NoError(err)
				reviewed, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
				require.NoError(err)
				changed, other := left, right
				if endpoint == "right" {
					changed, other = right, left
				}
				expand := func() {
					_, err := st.LinkParticipants(changed, extra)
					require.NoError(err)
				}
				switch stage {
				case "before_acceptance":
					expand()
					fresh, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
					require.NoError(err)
					assert.NotEqual(reviewed.ReviewToken, fresh.ReviewToken)
					listed, err := st.ListIdentityMatchReviewsContext(ctx, nil, 10, 0)
					require.NoError(err)
					require.Len(listed, 1)
					assert.Equal(fresh.ReviewToken, listed[0].ReviewToken)
				case "during_application":
					reset := st.SetIdentityMatchReviewAfterDecisionHookForTest(expand)
					defer reset()
				case "recovery":
					interrupted, cancel := context.WithCancel(ctx)
					defer cancel()
					reset := st.SetIdentityMatchReviewAfterDecisionHookForTest(cancel)
					_, _, err = st.DecideIdentityMatchReviewedContext(interrupted, candidate.ID,
						reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
					reset()
					require.ErrorIs(err, context.Canceled)
					expand()
				}
				if stage == "recovery" {
					_, _, _, err = st.ResumeAcceptedIdentityMatchCandidateContext(ctx, candidate.ID)
				} else {
					_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
						reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
				}
				require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
				current, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
				require.NoError(err)
				wantState := store.IdentityMatchStateConflict
				if stage == "before_acceptance" {
					wantState = store.IdentityMatchStateCandidate
				}
				assert.Equal(wantState, current.State)
				assert.False(current.ApplicationPending)
				members, err := st.ClusterMembers(other)
				require.NoError(err)
				assert.Equal([]int64{other}, members, "the earlier review must not link the expanded component")
				members, err = st.ClusterMembers(changed)
				require.NoError(err)
				assert.Equal([]int64{changed, extra}, members, "the independently created link must remain")
			})
		}
	}
}

func TestIdentityMatchReviewPreservesUnchangedComponentMembership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	var participants []int64
	for _, name := range []string{"left", "right", "member-a", "member-b", "unrelated-a", "unrelated-b"} {
		id, err := st.EnsureParticipantByIdentifier("beeper", name, "Synthetic Example")
		require.NoError(err)
		participants = append(participants, id)
	}
	left, right, memberA, memberB, unrelatedA, unrelatedB := participants[0], participants[1], participants[2], participants[3], participants[4], participants[5]
	_, err := st.LinkParticipants(left, memberA)
	require.NoError(err)
	_, err = st.LinkParticipants(memberA, memberB)
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	reviewed, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	_, err = st.LinkParticipants(unrelatedA, unrelatedB)
	require.NoError(err)
	unrelated, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(reviewed.ReviewToken, unrelated.ReviewToken)

	// Replace an edge while restoring the same endpoint component membership.
	_, err = st.UnlinkParticipants(left, memberA)
	require.NoError(err)
	_, err = st.LinkParticipants(left, memberB)
	require.NoError(err)
	rewired, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(reviewed.ReviewToken, rewired.ReviewToken)
	_, revision, err := st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	members, err := st.ClusterMembers(right)
	require.NoError(err)
	assert.Equal([]int64{left, right, memberA, memberB}, members)
	_, retryRevision, err := st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		reviewed.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err, "the receipt must bind to the successfully joined component")
	assert.Equal(revision, retryRevision)
}
