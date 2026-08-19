package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func trackedTestPerson(t *testing.T) (*store.Store, *store.Person) {
	t.Helper()
	fixture := storetest.New(t)
	participant := fixture.EnsureParticipant(
		"tracked@example.com", "tracked", "example.com")
	person, _, err := fixture.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return fixture.Store, person
}

func TestPersonTrackingIsIdempotentPresenceState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, person := trackedTestPerson(t)
	ctx := t.Context()

	state, err := st.GetPersonTrackingContext(ctx, person.ID)
	require.NoError(err)
	assert.False(state.Tracked)
	assert.Nil(state.TrackedAt)

	tracked, err := st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	assert.True(tracked.Tracked)
	require.NotNil(tracked.TrackedAt)
	firstTrackedAt := *tracked.TrackedAt

	again, err := st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	require.NotNil(again.TrackedAt)
	assert.Equal(firstTrackedAt, *again.TrackedAt)

	untracked, err := st.SetPersonTrackingContext(ctx, person.ID, false)
	require.NoError(err)
	assert.False(untracked.Tracked)
	assert.Nil(untracked.TrackedAt)

	againUntracked, err := st.SetPersonTrackingContext(ctx, person.ID, false)
	require.NoError(err)
	assert.False(againUntracked.Tracked)

	retracked, err := st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	require.NotNil(retracked.TrackedAt)
	assert.False(retracked.TrackedAt.Before(firstTrackedAt))

	unchanged, err := st.GetPersonContext(ctx, person.ID)
	require.NoError(err)
	assert.Equal(person.Revision, unchanged.Revision)
}

func TestPersonTrackingRejectsUnknownPersonAndCascadesOnDelete(t *testing.T) {
	require := require.New(t)
	st, person := trackedTestPerson(t)
	ctx := t.Context()

	_, err := st.GetPersonTrackingContext(ctx, person.ID+9999)
	require.ErrorIs(err, store.ErrPersonNotFound)
	_, err = st.SetPersonTrackingContext(ctx, person.ID+9999, true)
	require.ErrorIs(err, store.ErrPersonNotFound)

	_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(ctx, person.ID, person.Revision))
	_, err = st.GetPersonTrackingContext(ctx, person.ID)
	require.ErrorIs(err, store.ErrPersonNotFound)

	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM person_tracking WHERE person_id = ?`),
		person.ID).Scan(&count))
	assert.Zero(t, count)
}
