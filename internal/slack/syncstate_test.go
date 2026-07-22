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

func TestSyncStateMergePrefersAdvancedCursors(t *testing.T) {
	assert := assert.New(t)
	base := NewSyncState()
	bcs := base.EnsureConv("C01")
	bcs.Cursor = "100.000001"
	base.SweepWatermark, base.SweepOffset = "90.000001", 7200

	newer := NewSyncState()
	ncs := newer.EnsureConv("C01")
	ncs.Cursor = "200.000001"
	ncs.Done = true
	ncs.SweptThrough = "170.000001"
	ncs.PendingThreads = []PendingThread{{RootTS: "50.000001", DrainedTo: "60.000001", Forecast: 7}}
	newer.EnsureConv("C02").BackfillCursor = "opaque"
	newer.SweepWatermark, newer.SweepOffset = "180.000001", -14400

	base.Merge(newer)
	mcs := base.EnsureConv("C01")
	assert.Equal("200.000001", mcs.Cursor)
	assert.True(mcs.Done)
	assert.Equal("opaque", base.EnsureConv("C02").BackfillCursor)
	assert.Equal("180.000001", base.SweepWatermark, "the further-advanced watermark wins")
	assert.Equal(-14400, base.SweepOffset, "the winning watermark carries its audit offset")
	assert.Equal("170.000001", mcs.SweptThrough)
	assert.Equal([]PendingThread{{RootTS: "50.000001", DrainedTo: "60.000001", Forecast: 7}}, mcs.PendingThreads,
		"non-empty thread debt wins wholesale")

	// A stale checkpoint must never regress an advanced cursor, watermark,
	// or per-conversation certification stamp — and its EMPTY thread-debt
	// list must not clear live debt (a stale non-empty list only causes a
	// harmless idempotent re-drain; a wrongly cleared one loses replies).
	stale := NewSyncState()
	stale.EnsureConv("C01").Cursor = "150.000001"
	stale.EnsureConv("C01").SweptThrough = "110.000001"
	stale.SweepWatermark, stale.SweepOffset = "120.000001", 3600
	base.Merge(stale)
	assert.Equal("200.000001", base.EnsureConv("C01").Cursor)
	assert.Equal("180.000001", base.SweepWatermark)
	assert.Equal(-14400, base.SweepOffset)
	assert.Equal("170.000001", base.EnsureConv("C01").SweptThrough,
		"a stale checkpoint's certification stamp must not regress the advanced one")
	assert.Len(base.EnsureConv("C01").PendingThreads, 1,
		"an empty list must never clear outstanding thread debt")
}

func TestSyncStateMergeGenerationsSupersede(t *testing.T) {
	assert := assert.New(t)
	// A --full repair session resets the state under a bumped generation.
	// Merging must treat generations as lineage, not fields: a NEWER
	// generation wins wholesale (an interrupted repair's checkpoint
	// supersedes the pre-repair success blob — field-wise blending would
	// OR the old Done flags over fresh partial cursors and silently
	// abandon the repair), and an OLDER generation is ignored entirely.
	preRepair := NewSyncState()
	pcs := preRepair.EnsureConv("C01")
	pcs.Cursor = "200.000001"
	pcs.Done = true
	preRepair.SweepWatermark = "180.000001"

	repair := NewSyncState()
	repair.Generation = 1
	repair.RepairPending = true
	repair.EnsureConv("C01").BackfillCursor = "opaque-mid-repair"

	// Newer generation overlaid on older base: wholesale adoption.
	merged, err := LoadSyncState(mustMarshal(t, preRepair))
	require.NoError(t, err)
	merged.Merge(repair)
	assert.Equal(1, merged.Generation)
	assert.True(merged.RepairPending)
	assert.False(merged.EnsureConv("C01").Done, "pre-repair Done flags must not blend into the repair lineage")
	assert.Empty(merged.SweepWatermark)
	assert.Equal("opaque-mid-repair", merged.EnsureConv("C01").BackfillCursor)

	// Older generation overlaid on newer base: ignored entirely.
	merged2, err := LoadSyncState(mustMarshal(t, repair))
	require.NoError(t, err)
	merged2.Merge(preRepair)
	assert.False(merged2.EnsureConv("C01").Done, "pre-repair residue must not blend into the repair lineage")
	assert.Empty(merged2.SweepWatermark)
	assert.True(merged2.RepairPending)
}

func mustMarshal(t *testing.T, s *SyncState) string {
	t.Helper()
	blob, err := s.Marshal()
	require.NoError(t, err)
	return blob
}
