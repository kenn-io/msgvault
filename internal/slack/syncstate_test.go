package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := NewSyncState()
	cs := s.EnsureConv("C01")
	cs.Cursor = "100.000001"
	cs.Done = true
	s.SweepWatermark = "150.000001"
	s.SweepOffset = 7200

	blob, err := s.Marshal()
	require.NoError(err)
	loaded, err := LoadSyncState(blob)
	require.NoError(err)
	lcs := loaded.EnsureConv("C01")
	assert.Equal("100.000001", lcs.Cursor)
	assert.True(lcs.Done)
	assert.Equal("150.000001", loaded.SweepWatermark)
	assert.Equal(7200, loaded.SweepOffset)
}

func TestSyncStateLegacyThreadsBlobLoads(t *testing.T) {
	assert := assert.New(t)
	// Checkpoints written by the superseded thread-tracking design carry a
	// per-conversation "threads" map; they must load cleanly (the key is
	// simply ignored) so an upgrade never breaks resume.
	blob := `{"conversations":{"C01":{"cursor":"100.000001","done":true,"threads":{"50.000001":"60.000001"}}}}`
	loaded, err := LoadSyncState(blob)
	require.NoError(t, err)
	assert.Equal("100.000001", loaded.EnsureConv("C01").Cursor)
	assert.True(loaded.EnsureConv("C01").Done)
	assert.Empty(loaded.SweepWatermark, "legacy blobs start the sweep from the first-sweep floor")
}

func TestLoadSyncStateRejectsNullConversation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var state *SyncState
	var err error
	require.NotPanics(func() {
		state, err = LoadSyncState(`{"conversations":{"C01":null}}`)
	})
	require.Error(err)
	require.ErrorContains(err, `conversation "C01" is null`)
	assert.Nil(state)
}

func TestMCPNativeSlackProvenanceStateRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	state, err := LoadSyncState(`{"conversations":{"C01":{"cursor":"200.000000","done":true,"backfill_latest":"300.000000","backfill_live_after":"189.999999","pending_threads":[{"root":"100.000000","floor":"210.000000","drained_to":"220.000000","live_after":"200.000000"}]}}}`)
	require.NoError(err)
	cs := state.EnsureConv("C01")
	// A historical full audit expands coverage without expanding live evidence.
	cs.RecordPendingThread("100.000000", 9)
	cs.recordThreadLiveAfter("100.000000", "")
	blob, err := state.Marshal()
	require.NoError(err)
	resumed, err := LoadSyncState(blob)
	require.NoError(err)
	got := resumed.EnsureConv("C01")
	assert.Equal("189.999999", got.BackfillLiveAfter)
	require.Len(got.PendingThreads, 1)
	assert.Empty(got.PendingThreads[0].Floor)
	assert.Empty(got.PendingThreads[0].DrainedTo)
	assert.Equal("200.000000", got.PendingThreads[0].LiveAfter)
	// A later/narrower sweep must not erase earlier admitted observations.
	got.recordThreadLiveAfter("100.000000", "250.000000")
	assert.Equal("200.000000", got.PendingThreads[0].LiveAfter)
}

func TestMCPNativeSlackLegacyDebtIsHistorical(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	state, err := LoadSyncState(`{"conversations":{"C01":{"cursor":"200.000000","done":true,"backfill_latest":"300.000000","pending_threads":[{"root":"100.000000","drained_to":"220.000000"}]}}}`)
	require.NoError(err)
	cs := state.EnsureConv("C01")
	assert.Empty(cs.BackfillLiveAfter)
	require.Len(cs.PendingThreads, 1)
	assert.Empty(cs.PendingThreads[0].LiveAfter)
}
