package beeper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	s := NewSyncState()
	s.Chat("!a:x").Newest = "100"
	s.Chat("!a:x").Oldest = "5"
	s.Chat("!b:x").Done = true
	s.Anchor = &AnchorProbe{ChatID: "!a:x", MessageID: "42", Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	s.ListWatermark = "2026-01-02T03:04:05Z"

	blob, err := s.Marshal()
	require.NoError(t, err)

	got, err := LoadSyncState(blob)
	require.NoError(t, err)
	assert.Equal(t, s.Chats["!a:x"], got.Chats["!a:x"])
	assert.Equal(t, s.Chats["!b:x"], got.Chats["!b:x"])
	require.NotNil(t, got.Anchor)
	assert.Equal(t, "42", got.Anchor.MessageID)
	assert.True(t, got.Anchor.Timestamp.Equal(s.Anchor.Timestamp))
	assert.Equal(t, s.ListWatermark, got.ListWatermark)
}

func TestLoadSyncStateEmpty(t *testing.T) {
	s, err := LoadSyncState("")
	require.NoError(t, err)
	require.NotNil(t, s.Chats)
	assert.Empty(t, s.Chats)
	assert.Nil(t, s.Anchor)
}

func TestLoadSyncStateInvalid(t *testing.T) {
	_, err := LoadSyncState("{not json")
	require.Error(t, err)
}

func TestSyncStateMerge(t *testing.T) {
	base := NewSyncState()
	base.Chat("!a:x").Newest = "10"
	base.Chat("!a:x").Oldest = "5"
	base.Chat("!keep:x").Newest = "77"
	base.ListWatermark = "2026-01-01T00:00:00Z"
	base.Anchor = &AnchorProbe{ChatID: "!a:x", MessageID: "1"}

	// Checkpoint from an interrupted, more advanced run.
	cp := NewSyncState()
	cp.Chat("!a:x").Newest = "20"
	cp.Chat("!a:x").Done = true
	cp.Chat("!new:x").Oldest = "3"
	cp.ListWatermark = "2026-02-01T00:00:00Z"
	cp.Anchor = &AnchorProbe{ChatID: "!other:x", MessageID: "9"}

	base.Merge(cp)

	assert.Equal(t, "20", base.Chats["!a:x"].Newest, "checkpoint cursor wins")
	assert.Equal(t, "5", base.Chats["!a:x"].Oldest, "empty checkpoint field keeps baseline")
	assert.True(t, base.Chats["!a:x"].Done, "done flags OR")
	assert.Equal(t, "77", base.Chats["!keep:x"].Newest, "untouched chats survive")
	assert.Equal(t, "3", base.Chats["!new:x"].Oldest, "new chats copied")
	assert.Equal(t, "2026-02-01T00:00:00Z", base.ListWatermark, "later watermark wins")
	assert.Equal(t, "1", base.Anchor.MessageID, "existing anchor is never replaced")
}

func TestSyncStateMergeNil(t *testing.T) {
	base := NewSyncState()
	base.Chat("!a:x").Newest = "10"
	base.Merge(nil)
	assert.Equal(t, "10", base.Chats["!a:x"].Newest)
}

func TestSyncStateMergeFillsAnchor(t *testing.T) {
	base := NewSyncState()
	cp := NewSyncState()
	cp.Anchor = &AnchorProbe{ChatID: "!a:x", MessageID: "9"}
	base.Merge(cp)
	require.NotNil(t, base.Anchor)
	assert.Equal(t, "9", base.Anchor.MessageID)
}
