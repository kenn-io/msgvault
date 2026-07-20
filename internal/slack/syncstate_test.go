package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	s := NewSyncState()
	cs := s.EnsureConv("C01")
	cs.Cursor = "100.000001"
	cs.Done = true
	s.SweepWatermark = "150.000001"
	s.SweepOffset = 7200

	blob, err := s.Marshal()
	require.NoError(t, err)
	loaded, err := LoadSyncState(blob)
	require.NoError(t, err)
	lcs := loaded.EnsureConv("C01")
	assert.Equal(t, "100.000001", lcs.Cursor)
	assert.True(t, lcs.Done)
	assert.Equal(t, "150.000001", loaded.SweepWatermark)
	assert.Equal(t, 7200, loaded.SweepOffset)
}

func TestSyncStateLegacyThreadsBlobLoads(t *testing.T) {
	// Checkpoints written by the superseded thread-tracking design carry a
	// per-conversation "threads" map; they must load cleanly (the key is
	// simply ignored) so an upgrade never breaks resume.
	blob := `{"conversations":{"C01":{"cursor":"100.000001","done":true,"threads":{"50.000001":"60.000001"}}}}`
	loaded, err := LoadSyncState(blob)
	require.NoError(t, err)
	assert.Equal(t, "100.000001", loaded.EnsureConv("C01").Cursor)
	assert.True(t, loaded.EnsureConv("C01").Done)
	assert.Empty(t, loaded.SweepWatermark, "legacy blobs start the sweep from the first-sweep floor")
}

func TestSyncStateMergePrefersAdvancedCursors(t *testing.T) {
	base := NewSyncState()
	bcs := base.EnsureConv("C01")
	bcs.Cursor = "100.000001"
	base.SweepWatermark, base.SweepOffset = "90.000001", 7200

	newer := NewSyncState()
	ncs := newer.EnsureConv("C01")
	ncs.Cursor = "200.000001"
	ncs.Done = true
	newer.EnsureConv("C02").BackfillCursor = "opaque"
	newer.SweepWatermark, newer.SweepOffset = "180.000001", -14400

	base.Merge(newer)
	mcs := base.EnsureConv("C01")
	assert.Equal(t, "200.000001", mcs.Cursor)
	assert.True(t, mcs.Done)
	assert.Equal(t, "opaque", base.EnsureConv("C02").BackfillCursor)
	assert.Equal(t, "180.000001", base.SweepWatermark, "the further-advanced watermark wins")
	assert.Equal(t, -14400, base.SweepOffset, "the winning watermark carries its audit offset")

	// A stale checkpoint must never regress an advanced cursor or watermark.
	stale := NewSyncState()
	stale.EnsureConv("C01").Cursor = "150.000001"
	stale.SweepWatermark, stale.SweepOffset = "120.000001", 3600
	base.Merge(stale)
	assert.Equal(t, "200.000001", base.EnsureConv("C01").Cursor)
	assert.Equal(t, "180.000001", base.SweepWatermark)
	assert.Equal(t, -14400, base.SweepOffset)
}

func TestSyncStateMergeCursorUnitIsAtomic(t *testing.T) {
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
	assert.Equal(t, "150.000001", mcs.Cursor)
	assert.Empty(t, mcs.IncrCursor, "a completed window's cleared page cursor must win")
	assert.Empty(t, mcs.IncrMaxTS)

	// The reverse: a STALE checkpoint (cursor behind) must not inject its
	// mid-window state under the advanced cursor.
	stale := NewSyncState()
	scs := stale.EnsureConv("C01")
	scs.Cursor = "100.000001"
	scs.IncrCursor = "opaque-page-1"
	scs.IncrMaxTS = "120.000001"
	base.Merge(stale)
	mcs = base.EnsureConv("C01")
	assert.Equal(t, "150.000001", mcs.Cursor)
	assert.Empty(t, mcs.IncrCursor, "a stale checkpoint's page cursor must not pair with a newer window")
	assert.Empty(t, mcs.IncrMaxTS)
}
