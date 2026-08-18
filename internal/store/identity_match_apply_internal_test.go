package store

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
