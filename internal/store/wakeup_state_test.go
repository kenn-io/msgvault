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
	old, raw, err := st.LoadWakeupState(ctx, "one")
	require.NoError(err)
	assert.Nil(raw)
	next := store.WakeupState{At: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), AfterID: 4, AfterRow: true}
	ok, err := st.AdvanceWakeupState(ctx, "one", raw, next)
	require.NoError(err)
	require.True(ok)
	ok, err = st.AdvanceWakeupState(ctx, "one", raw, old)
	require.NoError(err)
	assert.False(ok)
	got, raw, err := st.LoadWakeupState(ctx, "one")
	require.NoError(err)
	assert.Equal(next, got)
	got.Failures = 1
	ok, err = st.AdvanceWakeupState(ctx, "one", raw, got)
	require.NoError(err)
	assert.True(ok)
	ok, err = st.AdvanceWakeupState(ctx, "one", raw, old)
	require.NoError(err)
	assert.False(ok)
	current, _, err := st.LoadWakeupState(ctx, "one")
	require.NoError(err)
	assert.Equal(got, current)
	ok, err = st.AdvanceWakeupState(ctx, "two", nil, next)
	require.NoError(err)
	require.True(ok)
	pruned, err := st.PruneWakeupStates(ctx, []string{"one"})
	require.NoError(err)
	assert.Equal(1, pruned)
	_, raw, err = st.LoadWakeupState(ctx, "two")
	require.NoError(err)
	assert.Nil(raw)
	_, raw, err = st.LoadWakeupState(ctx, "one")
	require.NoError(err)
	assert.NotNil(raw)
}
