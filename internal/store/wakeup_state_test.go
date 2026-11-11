package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWakeupStateCompareAndSwapAndPrune(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	ctx := t.Context()
	old, raw, err := st.LoadWakeupStateKey(ctx, store.WakeupStateKey("one"))
	require.NoError(err)
	assert.Nil(raw)
	next := store.WakeupState{At: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), AfterID: 4, AfterRow: true}
	ok, err := st.AdvanceWakeupStateKey(ctx, store.WakeupStateKey("one"), raw, next)
	require.NoError(err)
	require.True(ok)
	ok, err = st.AdvanceWakeupStateKey(ctx, store.WakeupStateKey("one"), raw, old)
	require.NoError(err)
	assert.False(ok)
	got, raw, err := st.LoadWakeupStateKey(ctx, store.WakeupStateKey("one"))
	require.NoError(err)
	assert.Equal(next, got)
	got.Failures = 1
	ok, err = st.AdvanceWakeupStateKey(ctx, store.WakeupStateKey("one"), raw, got)
	require.NoError(err)
	assert.True(ok)
	ok, err = st.AdvanceWakeupStateKey(ctx, store.WakeupStateKey("one"), raw, old)
	require.NoError(err)
	assert.False(ok)
	current, _, err := st.LoadWakeupStateKey(ctx, store.WakeupStateKey("one"))
	require.NoError(err)
	assert.Equal(got, current)
	ok, err = st.AdvanceWakeupStateKey(ctx, store.WakeupStateKey("two"), nil, next)
	require.NoError(err)
	require.True(ok)
	pruned, err := st.PruneWakeupStates(ctx, []string{"one"})
	require.NoError(err)
	assert.Equal(1, pruned)
	_, raw, err = st.LoadWakeupStateKey(ctx, store.WakeupStateKey("two"))
	require.NoError(err)
	assert.Nil(raw)
	_, raw, err = st.LoadWakeupStateKey(ctx, store.WakeupStateKey("one"))
	require.NoError(err)
	assert.NotNil(raw)
}

func TestEventsPositionSurvivesWakeupPruneAndNameCollision(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	position := store.WakeupState{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	key := store.MCPEventsPositionKey("sub-x")
	ok, err := st.AdvanceWakeupStateKey(ctx, key, nil, position)
	require.NoError(err)
	require.True(ok)
	ok, err = st.AdvanceWakeupStateKey(ctx, store.WakeupStateKey("sub-x"), nil, store.WakeupState{Failures: 2})
	require.NoError(err)
	require.True(ok)
	_, err = st.PruneWakeupStates(ctx, nil)
	require.NoError(err)
	got, raw, err := st.LoadWakeupStateKey(ctx, key)
	require.NoError(err)
	assert.NotNil(raw)
	assert.Equal(position, got)
	_, raw, err = st.LoadWakeupStateKey(ctx, store.WakeupStateKey("sub-x"))
	require.NoError(err)
	assert.Nil(raw)

	for _, key := range []string{store.WakeupStateKey("future"), store.MCPEventsPositionKey("future")} {
		const future = `{"at":"2026-01-01T00:00:00Z","future_field":true}`
		_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO archive_metadata (key,value) VALUES (?,?)`), key, future)
		require.NoError(err)
		_, _, err = st.LoadWakeupStateKey(ctx, key)
		require.Error(err)
		var unchanged string
		require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT value FROM archive_metadata WHERE key=?`), key).Scan(&unchanged))
		assert.Equal(future, unchanged)
	}
}
