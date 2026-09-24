package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonMatchBlockingSeedsMissingEmailPairOnlyOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "blocking-left", "Blocking Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("apple_id", "blocking-right", "Blocking Right")
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("beeper", "blocking-second")
	require.NoError(err)
	for _, item := range []struct{ participantID, sourceID int64 }{{left, fixture.Source.ID}, {right, otherSource.ID}} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', 'pair@example.test', 'pair@example.test', ?)`),
			item.participantID, item.sourceID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}
	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 2)
	require.NoError(err)
	assert.Equal(1, created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(left, candidates[0].LeftID)
	assert.Equal(right, candidates[0].RightID)
	require.Len(candidates[0].Evidence, 1)
	assert.Len(candidates[0].Evidence[0].SourceSupport, 2)
	created, err = st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 2)
	require.NoError(err)
	assert.Zero(created)
}

func TestPersonMatchBlockingSkipsParticipantsAlreadyConnected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "connected-left", "Connected Left")
	require.NoError(err)
	middle, err := st.EnsureParticipantByIdentifier("apple_id", "connected-middle", "Connected Middle")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("signal", "connected-right", "Connected Right")
	require.NoError(err)
	unlinked, err := st.EnsureParticipantByIdentifier("telegram", "unlinked-peer", "Unlinked Peer")
	require.NoError(err)
	_, err = st.LinkParticipants(left, middle)
	require.NoError(err)
	_, err = st.LinkParticipants(middle, right)
	require.NoError(err)
	for _, participantID := range []int64{left, right, unlinked} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_contact_observations
			(participant_id, source_id, address_kind, original_value, normalized_value, source)
			VALUES (?, ?, 'email', 'connected@example.test', 'connected@example.test', ?)`),
			participantID, fixture.Source.ID, store.ProvenanceArchiveObservation)
		require.NoError(err)
	}

	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Equal(1, created)
	candidates, err := st.ListIdentityMatchReviewsContext(t.Context(), []store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(left, candidates[0].LeftID)
	assert.Equal(unlinked, candidates[0].RightID)
}
