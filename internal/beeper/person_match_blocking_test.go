package beeper

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonMatchBlockingPreservesBeeperConflictAndAdvances(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	recorder, matcher := newObservationRecorder(st), newIdentityMatcher(st)
	for _, item := range []struct{ account, network, user, email string }{
		{"first-telegram", "Telegram", "@first-a:example.test", "a@example.test"},
		{"second-telegram", "Telegram", "@first-b:example.test", "a@example.test"},
		{"later-telegram", "Telegram", "@later-a:example.test", "z@example.test"},
		{"later-imessage", "iMessage", "@later-b:example.test", "z@example.test"},
	} {
		participant, err := st.EnsureParticipantByIdentifier(participantIdentifierType, item.user, "Synthetic Example")
		require.NoError(err)
		captureAndMatch(t, recorder, matcher, participant, &User{ID: item.user, Email: item.email}, captureContext{
			SourceID: newBeeperTestSource(t, st, item.account), AccountID: item.account, Network: item.network,
		})
	}
	before, err := st.ListIdentityMatchReviewsContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(before, 1)
	require.Equal(store.IdentityMatchStateConflict, before[0].State)
	require.Equal(new("telegram"), before[0].ServiceSlug)
	require.Len(before[0].SourceSupport, 1)

	created, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Zero(created, "complete the existing conflict instead of creating an unscoped suggestion")
	after, err := st.ListIdentityMatchReviewsContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(after, 1)
	assert.Equal(before[0].ID, after[0].ID)
	assert.Equal(store.IdentityMatchStateConflict, after[0].State)
	assert.Equal(before[0].ServiceSlug, after[0].ServiceSlug)
	assert.Len(after[0].SourceSupport, 2)

	created, err = st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Equal(1, created, "the next bounded pass must reach the unrelated email pair")
	after, err = st.ListIdentityMatchReviewsContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(after, 2)
	assert.Equal(store.IdentityMatchStateConflict, after[0].State)
	assert.Equal(store.IdentityMatchStateCandidate, after[1].State)
	assert.Equal(new("z@example.test"), after[1].NormalizedValue)
	assert.Nil(after[1].ServiceSlug)
	created, err = st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 1)
	require.NoError(err)
	assert.Zero(created)
}
