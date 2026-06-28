package teams

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := NewSyncState()
	s.SetChatCursor("19:abc@thread.v2", "2026-01-01T00:00:00Z")
	s.SetChannelDelta("team1/chanA", "https://graph/delta?token=xyz")

	blob, err := s.Marshal()
	require.NoError(err)

	got, err := LoadSyncState(blob)
	require.NoError(err)
	assert.Equal("2026-01-01T00:00:00Z", got.ChatCursor("19:abc@thread.v2"))
	assert.Equal("https://graph/delta?token=xyz", got.ChannelDelta("team1/chanA"))
	assert.Empty(got.ChatCursor("unknown"))
}

func TestLoadSyncStateEmpty(t *testing.T) {
	got, err := LoadSyncState("")
	require.NoError(t, err)
	assert.Empty(t, got.ChatCursor("anything"))
	assert.Empty(t, got.ChannelDelta("anything"))
}

func TestLoadSyncStateInvalid(t *testing.T) {
	_, err := LoadSyncState("{not json")
	require.Error(t, err)
}

func TestSyncStateMerge(t *testing.T) {
	assert := assert.New(t)

	// baseline: chatA=t1, channel key1=d1
	baseline := NewSyncState()
	baseline.SetChatCursor("chatA", "2025-01-01T00:00:00.000000000Z")
	baseline.SetChannelDelta("key1", "https://delta/d1")

	// other (checkpoint): chatA=t2 (later), chatB=t3, channel key1=d2
	other := NewSyncState()
	other.SetChatCursor("chatA", "2025-06-01T00:00:00.000000000Z")
	other.SetChatCursor("chatB", "2025-03-01T00:00:00.000000000Z")
	other.SetChannelDelta("key1", "https://delta/d2")

	baseline.Merge(other)

	// chatA: other's value is lexicographically greater — use it
	assert.Equal("2025-06-01T00:00:00.000000000Z", baseline.ChatCursor("chatA"))
	// chatB: only in other — should be picked up
	assert.Equal("2025-03-01T00:00:00.000000000Z", baseline.ChatCursor("chatB"))
	// key1 channel: other's deltaLink preferred when present
	assert.Equal("https://delta/d2", baseline.ChannelDelta("key1"))
}

func TestSyncStateMergeNilOther(t *testing.T) {
	s := NewSyncState()
	s.SetChatCursor("chatA", "2025-01-01T00:00:00Z")
	// Merging nil must not panic
	s.Merge(nil)
	assert.Equal(t, "2025-01-01T00:00:00Z", s.ChatCursor("chatA"))
}

func TestSyncStateMergeBaselineWins(t *testing.T) {
	assert := assert.New(t)

	baseline := NewSyncState()
	baseline.SetChatCursor("chatA", "2025-06-01T00:00:00.000000000Z") // later

	other := NewSyncState()
	other.SetChatCursor("chatA", "2025-01-01T00:00:00.000000000Z") // earlier

	baseline.Merge(other)
	// baseline already has the later value — it should win
	assert.Equal("2025-06-01T00:00:00.000000000Z", baseline.ChatCursor("chatA"))
}

func TestSyncStateMergeParsesVariablePrecisionTimestamps(t *testing.T) {
	baseline := NewSyncState()
	baseline.SetChatCursor("chatA", "2025-01-01T00:00:00Z")

	other := NewSyncState()
	other.SetChatCursor("chatA", "2025-01-01T00:00:00.1Z")

	baseline.Merge(other)
	assert.Equal(t, "2025-01-01T00:00:00.1Z", baseline.ChatCursor("chatA"))
}
