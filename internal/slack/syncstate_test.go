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

func TestSyncStateMergeCursorUnitIsAtomic(t *testing.T) {
	assert := assert.New(t)
	// A baseline holding an interrupted window (Cursor + IncrCursor pair)
	// merged with a NEWER checkpoint that completed the window (advanced
	// Cursor, cleared IncrCursor): the clear is authoritative. Keeping the
	// stale page cursor would pair it with a window bound it was not minted
	// under — invalid pagination, possible cursor regression via stale
	// IncrMaxTS.
	base := NewSyncState()
	bcs := base.EnsureConv("C01")
	bcs.Cursor = "100.000001"
	bcs.IncrCursor = "opaque-page-3"
	bcs.IncrMaxTS = "150.000001"

	newer := NewSyncState()
	ncs := newer.EnsureConv("C01")
	ncs.Cursor = "150.000001" // window completed: cursor advanced, incr cleared

	base.Merge(newer)
	mcs := base.EnsureConv("C01")
	assert.Equal("150.000001", mcs.Cursor)
	assert.Empty(mcs.IncrCursor, "a completed window's cleared page cursor must win")
	assert.Empty(mcs.IncrMaxTS)

	// The reverse: a STALE checkpoint (cursor behind) must not inject its
	// mid-window state under the advanced cursor.
	stale := NewSyncState()
	scs := stale.EnsureConv("C01")
	scs.Cursor = "100.000001"
	scs.IncrCursor = "opaque-page-1"
	scs.IncrMaxTS = "120.000001"
	base.Merge(stale)
	mcs = base.EnsureConv("C01")
	assert.Equal("150.000001", mcs.Cursor)
	assert.Empty(mcs.IncrCursor, "a stale checkpoint's page cursor must not pair with a newer window")
	assert.Empty(mcs.IncrMaxTS)
}
