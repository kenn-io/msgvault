package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewedIdentityMatchRequiresFreshReviewAfterCandidateCollapse(t *testing.T) {
	for _, test := range []struct {
		name           string
		reviewSurvivor bool
		restart        bool
	}{
		{name: "reviewed loser"},
		{name: "reviewed survivor", reviewSurvivor: true},
		{name: "reviewed loser after restart", restart: true},
		{name: "reviewed survivor after restart", reviewSurvivor: true, restart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dbPath := filepath.Join(t.TempDir(), "review-collapse.db")
			st, err := OpenForTest(dbPath)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			require.NoError(st.InitSchema())
			absorbed, err := st.EnsureParticipantByIdentifier("beeper", "@absorbed:example.test", "Absorbed Example")
			require.NoError(err)
			survivor, err := st.EnsureParticipantByIdentifier("beeper", "@survivor:example.test", "Survivor Example")
			require.NoError(err)
			other, err := st.EnsureParticipantByIdentifier("beeper", "@other:example.test", "Other Example")
			require.NoError(err)
			winner, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
				LeftKind: IdentityMatchParticipant, LeftID: survivor,
				RightKind: IdentityMatchParticipant, RightID: other,
				Basis: IdentityMatchEmail, State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
			})
			require.NoError(err)
			loser, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
				LeftKind: IdentityMatchParticipant, LeftID: absorbed,
				RightKind: IdentityMatchParticipant, RightID: other,
				Basis: IdentityMatchEmail, State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
			})
			require.NoError(err)
			reviewedID, unreviewedID := loser.ID, winner.ID
			if test.reviewSurvivor {
				reviewedID, unreviewedID = winner.ID, loser.ID
			}
			_, err = st.AddIdentityMatchEvidenceContext(t.Context(), unreviewedID, IdentityMatchEvidenceInput{
				EvidenceKind: "email", Detail: new("unreviewed@example.test"), Source: ProvenanceArchiveObservation,
			})
			require.NoError(err)
			review, err := st.GetIdentityMatchReviewContext(t.Context(), reviewedID)
			require.NoError(err)
			decisionCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			st.identityMatchReviewAfterDecisionHook = func() {
				require.NoError(st.MergeParticipants(absorbed, survivor))
				if test.restart {
					cancel()
				}
			}
			_, _, err = st.DecideIdentityMatchReviewedContext(decisionCtx, reviewedID,
				review.ReviewToken, IdentityMatchStateAccepted, nil)
			st.identityMatchReviewAfterDecisionHook = nil
			if test.restart {
				require.ErrorIs(err, context.Canceled)
				require.NoError(st.Close())
				st, err = OpenForTest(dbPath)
				require.NoError(err)
			} else {
				require.ErrorIs(err, ErrIdentityMatchReviewStale)
			}
			applied, err := st.ApplyAcceptedIdentityMatchesContext(t.Context(), 10)
			require.NoError(err)
			assert.Zero(applied)
			members, err := st.ClusterMembers(survivor)
			require.NoError(err)
			assert.NotContains(members, other)
			fresh, err := st.GetIdentityMatchReviewContext(t.Context(), winner.ID)
			require.NoError(err)
			assert.Equal(IdentityMatchStateConflict, fresh.State)
			assert.True(fresh.Actionable)
			assert.False(fresh.ApplicationPending)
			_, _, err = st.DecideIdentityMatchReviewedContext(t.Context(), winner.ID,
				review.ReviewToken, IdentityMatchStateAccepted, nil)
			require.ErrorIs(err, ErrIdentityMatchReviewStale)
			_, _, err = st.DecideIdentityMatchReviewedContext(t.Context(), winner.ID,
				fresh.ReviewToken, IdentityMatchStateAccepted, nil)
			require.NoError(err)
			members, err = st.ClusterMembers(survivor)
			require.NoError(err)
			assert.Contains(members, other)
		})
	}
}

func TestApplyAcceptedIdentityMatchesDoesNotRebuildConnectivityPerSatisfiedCandidate(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "accepted-replay.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())

	for i := range 3 {
		left, err := st.EnsureParticipantByIdentifier(
			"beeper", "@replay-left-"+string(rune('a'+i))+":beeper.local", "Replay")
		require.NoError(err)
		right, err := st.EnsureParticipantByIdentifier(
			"beeper", "@replay-right-"+string(rune('a'+i))+":beeper.local", "Replay")
		require.NoError(err)
		providerID := "provider-" + string(rune('a'+i))
		candidate, _, err := st.UpsertIdentityMatchCandidateContext(
			t.Context(), IdentityMatchCandidateInput{
				LeftKind: IdentityMatchParticipant, LeftID: left,
				RightKind: IdentityMatchParticipant, RightID: right,
				Basis: IdentityMatchStableProviderID, NormalizedValue: &providerID,
				State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
			})
		require.NoError(err)
		_, _, err = st.AcceptIdentityMatchCandidateContext(
			t.Context(), candidate.ID, "system", nil)
		require.NoError(err)
	}

	originalBuildAdjacency := buildAdjacency
	t.Cleanup(func() { buildAdjacency = originalBuildAdjacency })
	builds := 0
	buildAdjacency = func(edges []linkEdge) map[int64][]int64 {
		builds++
		return originalBuildAdjacency(edges)
	}

	applied, err := st.ApplyAcceptedIdentityMatchesContext(t.Context(), 1)
	require.NoError(err)
	assert.Zero(applied)
	assert.Zero(builds,
		"steady-state replay must not load the link graph when no candidate is pending")
}

func TestApplyAcceptedIdentityMatchFollowsParticipantMergeCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "accepted-merge-collision.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())

	absorbed, err := st.EnsureParticipantByIdentifier(
		"beeper", "@collision-absorbed:beeper.local", "Test User")
	require.NoError(err)
	survivor, err := st.EnsureParticipantByIdentifier(
		"beeper", "@collision-survivor:beeper.local", "Test User")
	require.NoError(err)
	other, err := st.EnsureParticipantByIdentifier(
		"beeper", "@collision-other:beeper.local", "Test User")
	require.NoError(err)
	providerID := "provider-collision"

	survivingCandidate, created, err := st.UpsertIdentityMatchCandidateContext(
		t.Context(), IdentityMatchCandidateInput{
			LeftKind: IdentityMatchParticipant, LeftID: survivor,
			RightKind: IdentityMatchParticipant, RightID: other,
			Basis: IdentityMatchStableProviderID, NormalizedValue: &providerID,
			State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
		})
	require.NoError(err)
	require.True(created)
	acceptedCandidate, created, err := st.UpsertIdentityMatchCandidateContext(
		t.Context(), IdentityMatchCandidateInput{
			LeftKind: IdentityMatchParticipant, LeftID: absorbed,
			RightKind: IdentityMatchParticipant, RightID: other,
			Basis: IdentityMatchStableProviderID, NormalizedValue: &providerID,
			State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
		})
	require.NoError(err)
	require.True(created)
	accepted, err := st.DecideIdentityMatchCandidateContext(
		t.Context(), acceptedCandidate.ID, IdentityMatchStateAccepted, "system", nil)
	require.NoError(err)
	require.Greater(accepted.ID, survivingCandidate.ID,
		"the pending acceptance must be the collision row that loses by ID")

	require.NoError(st.MergeParticipants(absorbed, survivor))
	_, err = st.GetIdentityMatchCandidateContext(t.Context(), accepted.ID)
	require.ErrorIs(err, ErrIdentityMatchNotFound,
		"participant merge must reproduce the stale accepted snapshot")

	applied, _, linked, err := st.applyAcceptedIdentityMatchCandidateContext(
		t.Context(), accepted, "system")
	require.NoError(err)
	assert.Equal(survivingCandidate.ID, applied.ID)
	assert.True(linked, "the surviving accepted candidate must be applied")
	members, err := st.ClusterMembers(survivor)
	require.NoError(err)
	assert.True(slices.Contains(members, other))
	reloaded, err := st.GetIdentityMatchCandidateContext(t.Context(), survivingCandidate.ID)
	require.NoError(err)
	assert.Equal(IdentityMatchStateAccepted, reloaded.State)
	assert.False(reloaded.applicationPending)
}

func TestApplyAcceptedIdentityMatchDoesNotAssumeMissingCandidateWasMerged(t *testing.T) {
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "accepted-missing-candidate.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@missing-left:beeper.local", "Test User")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@missing-right:beeper.local", "Test User")
	require.NoError(err)
	providerID := "provider-missing"
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		t.Context(), IdentityMatchCandidateInput{
			LeftKind: IdentityMatchParticipant, LeftID: left,
			RightKind: IdentityMatchParticipant, RightID: right,
			Basis: IdentityMatchStableProviderID, NormalizedValue: &providerID,
			State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
		})
	require.NoError(err)
	require.True(created)
	accepted, err := st.DecideIdentityMatchCandidateContext(
		t.Context(), candidate.ID, IdentityMatchStateAccepted, "system", nil)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`DELETE FROM identity_match_candidates WHERE id = ?`), candidate.ID)
	require.NoError(err, "simulate an unrecorded candidate removal")

	applied, _, linked, err := st.applyAcceptedIdentityMatchCandidateContext(
		t.Context(), accepted, "system")
	require.ErrorIs(err, ErrIdentityMatchNotFound,
		"a missing candidate is not proof that its endpoints were merged")
	assert.Nil(t, applied)
	assert.False(t, linked)
}
