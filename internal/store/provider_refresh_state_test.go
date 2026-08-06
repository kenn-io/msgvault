package store_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestProviderIdentityRefreshStateRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "user@example.test")
	require.NoError(err)

	_, found, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	assert.False(found, "a source that never refreshed has no state")

	require.NoError(st.RecordProviderIdentityRefreshOutcomeContext(t.Context(), source.ID, nil))
	state, found, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	require.True(found)
	assert.Empty(state.LastError)
	assert.WithinDuration(time.Now(), state.LastSuccessAt, time.Minute)
	assert.True(state.Fresh(time.Now(), time.Hour))
}

func TestProviderIdentityRefreshFailurePreservesLastSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "user@example.test")
	require.NoError(err)

	require.NoError(st.RecordProviderIdentityRefreshOutcomeContext(t.Context(), source.ID, nil))
	require.NoError(st.RecordProviderIdentityRefreshOutcomeContext(
		t.Context(), source.ID, errors.New("provider unavailable"),
	))

	state, found, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	require.True(found)
	assert.Equal("provider unavailable", state.LastError)
	assert.WithinDuration(time.Now(), state.FailedAt, time.Minute)
	assert.False(state.LastSuccessAt.IsZero(),
		"a failure keeps measuring staleness from the last refresh that worked")
	assert.False(state.Fresh(time.Now(), time.Hour), "a failed refresh is always due")

	require.NoError(st.RecordProviderIdentityRefreshOutcomeContext(t.Context(), source.ID, nil))
	state, found, err = st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	require.True(found)
	assert.Empty(state.LastError, "a success clears the previous failure")
	assert.True(state.Fresh(time.Now(), time.Hour))
}

func TestProviderIdentityRefreshStateFreshness(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		state store.ProviderIdentityRefreshState
		want  bool
	}{
		{name: "never succeeded", state: store.ProviderIdentityRefreshState{}, want: false},
		{
			name:  "recent success",
			state: store.ProviderIdentityRefreshState{LastSuccessAt: now.Add(-time.Minute)},
			want:  true,
		},
		{
			name:  "stale success",
			state: store.ProviderIdentityRefreshState{LastSuccessAt: now.Add(-2 * time.Hour)},
			want:  false,
		},
		{
			name: "recent success then failure",
			state: store.ProviderIdentityRefreshState{
				LastSuccessAt: now.Add(-time.Minute),
				LastError:     "provider unavailable",
				FailedAt:      now,
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.Fresh(now, time.Hour))
		})
	}
}

func TestProviderIdentityRefreshStateUndecodablePayloadIsDue(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "user@example.test")
	require.NoError(err)

	_, err = st.DB().Exec(
		st.Rebind(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)`),
		"provider_identity_refresh:"+strconv.FormatInt(source.ID, 10), "not json",
	)
	require.NoError(err)

	state, found, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	assert.True(found)
	assert.False(state.Fresh(time.Now(), time.Hour),
		"a corrupted row must trigger a refresh, which heals it")

	require.NoError(st.RecordProviderIdentityRefreshOutcomeContext(t.Context(), source.ID, nil))
	state, found, err = st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	require.True(found)
	assert.True(state.Fresh(time.Now(), time.Hour))
}

func TestProviderIdentityRefreshStateRejectsNonPositiveSourceID(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, _, err := st.ProviderIdentityRefreshStateContext(t.Context(), 0)
	require.ErrorContains(t, err, "source ID must be positive")
	err = st.RecordProviderIdentityRefreshOutcomeContext(t.Context(), -1, nil)
	require.ErrorContains(t, err, "source ID must be positive")
}
