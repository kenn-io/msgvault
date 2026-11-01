package beeper

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSyncStateEmpty(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, err := LoadSyncState("")
	require.NoError(err)
	require.NotNil(s.Chats)
	assert.Empty(s.Chats)
	assert.Empty(s.Anchors)
}

func TestSyncStateMerge(t *testing.T) {
	assert := assert.New(t)
	base := NewSyncState()
	base.EnsureChat("!a:x").Newest = "10"
	base.EnsureChat("!a:x").Oldest = "5"
	base.EnsureChat("!keep:x").Newest = "77"
	base.ListWatermark = "2026-01-01T00:00:00Z"
	base.Anchors = []AnchorProbe{{ChatID: "!a:x", MessageID: "1"}}

	// Checkpoint from an interrupted, more advanced run.
	cp := NewSyncState()
	cp.EnsureChat("!a:x").Newest = "20"
	cp.EnsureChat("!a:x").Done = true
	cp.EnsureChat("!new:x").Oldest = "3"
	cp.ListWatermark = "2026-02-01T00:00:00Z"
	cp.Anchors = []AnchorProbe{{ChatID: "!other:x", MessageID: "9"}}

	base.Merge(cp)

	assert.Equal("20", base.Chats["!a:x"].Newest, "checkpoint cursor wins")
	assert.Equal("5", base.Chats["!a:x"].Oldest, "empty checkpoint field keeps baseline")
	assert.True(base.Chats["!a:x"].Done, "the checkpoint completion state wins")
	assert.Equal("77", base.Chats["!keep:x"].Newest, "untouched chats survive")
	assert.Equal("3", base.Chats["!new:x"].Oldest, "new chats copied")
	assert.Equal("2026-02-01T00:00:00Z", base.ListWatermark, "later watermark wins")
	assert.Equal("1", base.Anchors[0].MessageID, "existing anchors are never replaced")
}

func TestSyncStateMergePreservesExplicitDoneReset(t *testing.T) {
	assert := assert.New(t)
	base := NewSyncState()
	base.EnsureChat("!reopened:x").Done = true

	checkpoint := NewSyncState()
	checkpoint.EnsureChat("!reopened:x").Oldest = "older-page"

	base.Merge(checkpoint)

	assert.False(base.Chats["!reopened:x"].Done,
		"a newer checkpoint can reopen a previously completed chat for backfill")
}

func TestSyncStateMergeNil(t *testing.T) {
	base := NewSyncState()
	base.EnsureChat("!a:x").Newest = "10"
	base.Merge(nil)
	assert.Equal(t, "10", base.Chats["!a:x"].Newest)
}

func TestSyncStateMergeFillsAnchor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := NewSyncState()
	cp := NewSyncState()
	cp.Anchors = []AnchorProbe{{ChatID: "!a:x", MessageID: "9"}}
	base.Merge(cp)
	require.NotEmpty(base.Anchors)
	assert.Equal("9", base.Anchors[0].MessageID)
}

func TestSyncStateMergePreservesExplicitTailScanClears(t *testing.T) {
	assert := assert.New(t)
	base := NewSyncState()
	base.TailScanStarted = "2026-01-01T00:00:00Z"
	base.EnsureChat("!a:x").TailProbeCursor = "old-page"

	checkpoint := NewSyncState()
	checkpoint.EnsureChat("!a:x")

	base.Merge(checkpoint)

	assert.Empty(base.TailScanStarted, "the newer checkpoint explicitly ended its scan")
	assert.Empty(base.Chats["!a:x"].TailProbeCursor, "the newer checkpoint cleared the tail probe cursor")
}
