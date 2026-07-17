package beeper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := NewSyncState()
	s.EnsureChat("!a:x").Newest = "100"
	s.EnsureChat("!a:x").Oldest = "5"
	s.EnsureChat("!b:x").Done = true
	s.Anchors = []AnchorProbe{{ChatID: "!a:x", MessageID: "42", Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}
	s.ListWatermark = "2026-01-02T03:04:05Z"

	blob, err := s.Marshal()
	require.NoError(err)

	got, err := LoadSyncState(blob)
	require.NoError(err)
	assert.Equal(s.Chats["!a:x"], got.Chats["!a:x"])
	assert.Equal(s.Chats["!b:x"], got.Chats["!b:x"])
	require.Len(got.Anchors, 1)
	assert.Equal("42", got.Anchors[0].MessageID)
	assert.True(got.Anchors[0].Timestamp.Equal(s.Anchors[0].Timestamp))
	assert.Equal(s.ListWatermark, got.ListWatermark)
}

func TestLoadSyncStateEmpty(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, err := LoadSyncState("")
	require.NoError(err)
	require.NotNil(s.Chats)
	assert.Empty(s.Chats)
	assert.Empty(s.Anchors)
}

func TestLoadSyncStateInvalid(t *testing.T) {
	_, err := LoadSyncState("{not json")
	require.Error(t, err)
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
	assert.True(base.Chats["!a:x"].Done, "done flags OR")
	assert.Equal("77", base.Chats["!keep:x"].Newest, "untouched chats survive")
	assert.Equal("3", base.Chats["!new:x"].Oldest, "new chats copied")
	assert.Equal("2026-02-01T00:00:00Z", base.ListWatermark, "later watermark wins")
	assert.Equal("1", base.Anchors[0].MessageID, "existing anchors are never replaced")
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
