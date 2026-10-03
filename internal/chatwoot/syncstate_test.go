package chatwoot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRangeSubtractPreservesUnseenBackdatedIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	remaining, err := subtractHandled(idRange{After: 1, Before: 101}, []int64{80, 3, 80, 42})
	require.NoError(err)
	assert.Equal([]idRange{{1, 3}, {4, 42}, {43, 80}, {81, 101}}, remaining)
	_, err = subtractHandled(idRange{After: 1, Before: 10}, []int64{10})
	require.Error(err)
	remaining, err = subtractHandled(idRange{After: 1, Before: 4}, []int64{3, 1, 2})
	require.NoError(err)
	assert.Empty(remaining)
}

func TestStateRejectsForeignScopeAndCorruptRanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	state := newSyncState("example/account/inbox")
	state.Conversations["42"] = &conversationState{Pending: []idRange{{1, 10}}, HighWater: 9, Artifacts: map[string]bool{}}
	blob, err := state.marshal()
	require.NoError(err)
	loaded, err := parseSyncState(blob, "example/account/inbox")
	require.NoError(err)
	assert.Equal(state, loaded)
	_, err = parseSyncState(blob, "other/account/inbox")
	require.Error(err)
	_, err = parseSyncState(`{"version":1,"scope":"example/account/inbox","conversations":{"42":{"pending":[{"after":9,"before":3}]}}}`, "example/account/inbox")
	require.Error(err)
}
