package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWakeupStateCompareAndSwapAndPrune(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	old, raw, err := st.LoadWakeupState(ctx, "one")
	require.NoError(t, err)
	assert.Nil(t, raw)
	next := store.WakeupState{At: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), AfterID: 4, AfterRow: true}
	ok, err := st.AdvanceWakeupState(ctx, "one", raw, next)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = st.AdvanceWakeupState(ctx, "one", raw, old)
	require.NoError(t, err)
	assert.False(t, ok)
	got, raw, err := st.LoadWakeupState(ctx, "one")
	require.NoError(t, err)
	assert.Equal(t, next, got)
	got.Failures = 1
	ok, err = st.AdvanceWakeupState(ctx, "one", raw, got)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = st.AdvanceWakeupState(ctx, "two", nil, next)
	require.NoError(t, err)
	require.True(t, ok)
	pruned, err := st.PruneWakeupStates(ctx, []string{"one"})
	require.NoError(t, err)
	assert.Equal(t, 1, pruned)
	_, raw, err = st.LoadWakeupState(ctx, "two")
	require.NoError(t, err)
	assert.Nil(t, raw)
	_, raw, err = st.LoadWakeupState(ctx, "one")
	require.NoError(t, err)
	assert.NotNil(t, raw)
}
