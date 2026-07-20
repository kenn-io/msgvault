package slack

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	s := NewSyncState()
	cs := s.EnsureConv("C01")
	cs.Cursor = "100.000001"
	cs.Done = true
	cs.TrackThread("50.000001", "60.000001")

	blob, err := s.Marshal()
	require.NoError(t, err)
	loaded, err := LoadSyncState(blob)
	require.NoError(t, err)
	lcs := loaded.EnsureConv("C01")
	assert.Equal(t, "100.000001", lcs.Cursor)
	assert.True(t, lcs.Done)
	assert.Equal(t, "60.000001", lcs.Threads["50.000001"])
}

func TestSyncStateMergePrefersAdvancedCursors(t *testing.T) {
	base := NewSyncState()
	bcs := base.EnsureConv("C01")
	bcs.Cursor = "100.000001"
	bcs.TrackThread("50.000001", "60.000001")

	newer := NewSyncState()
	ncs := newer.EnsureConv("C01")
	ncs.Cursor = "200.000001"
	ncs.Done = true
	ncs.TrackThread("50.000001", "70.000001")
	newer.EnsureConv("C02").BackfillCursor = "opaque"

	base.Merge(newer)
	mcs := base.EnsureConv("C01")
	assert.Equal(t, "200.000001", mcs.Cursor)
	assert.True(t, mcs.Done)
	assert.Equal(t, "70.000001", mcs.Threads["50.000001"])
	assert.Equal(t, "opaque", base.EnsureConv("C02").BackfillCursor)

	// A stale checkpoint must never regress an advanced cursor.
	stale := NewSyncState()
	stale.EnsureConv("C01").Cursor = "150.000001"
	stale.EnsureConv("C01").Threads = map[string]string{"50.000001": "65.000001"}
	base.Merge(stale)
	assert.Equal(t, "200.000001", base.EnsureConv("C01").Cursor)
	assert.Equal(t, "70.000001", base.EnsureConv("C01").Threads["50.000001"])
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

func TestPruneThreads(t *testing.T) {
	cs := &ConvState{Threads: map[string]string{}}
	oldPolled := tsFromTime(time.Now().Add(-60 * 24 * time.Hour))
	oldSkipped := tsFromTime(time.Now().Add(-59 * 24 * time.Hour))
	fresh := tsFromTime(time.Now().Add(-time.Hour))
	cs.TrackThread(oldPolled, "")
	cs.TrackThread(oldSkipped, "")
	cs.TrackThread(fresh, "")

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	cs.PruneThreads(cutoff, map[string]bool{oldPolled: true, fresh: true})
	assert.NotContains(t, cs.Threads, oldPolled, "polled roots past the lookback are pruned")
	assert.Contains(t, cs.Threads, oldSkipped, "unpolled roots must survive pruning or their replies are lost")
	assert.Contains(t, cs.Threads, fresh, "fresh roots stay tracked")
}

func tsFromTime(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10) + ".000100"
}
