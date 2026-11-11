package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPEventsCatalogCASAndPositionDeletion(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	initial := store.WakeupState{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ok, err := st.SwapMCPEventsCatalog(ctx, nil, []byte(`[{"id":"one"}]`), "one", &initial, "")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = st.SwapMCPEventsCatalog(ctx, nil, []byte(`[]`), "", nil, "")
	require.NoError(t, err)
	assert.False(t, ok)
	raw, err := st.LoadMCPEventsCatalog(ctx)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"id":"one"}]`, string(raw))
	state, _, err := st.LoadWakeupStateKey(ctx, store.MCPEventsPositionKey("one"))
	require.NoError(t, err)
	assert.Equal(t, initial, state)
	ok, err = st.SwapMCPEventsCatalog(ctx, raw, []byte(`[]`), "", nil, "one")
	require.NoError(t, err)
	require.True(t, ok)
	_, position, err := st.LoadWakeupStateKey(ctx, store.MCPEventsPositionKey("one"))
	require.NoError(t, err)
	assert.Nil(t, position)
}
