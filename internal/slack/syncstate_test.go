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
