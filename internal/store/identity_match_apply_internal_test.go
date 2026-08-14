package store

import (
	"path/filepath"
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
