package teams

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reset checkpoints are authoritative for chat history, even when a previous
// successful run has a newer cursor. Channel tokens retain their merge contract.
func FuzzSyncStateResetChatBaseline(f *testing.F) {
	f.Add("chat", "2026-01-01T00:00:00Z")
	f.Add("", "")
	f.Add("opaque:\x00/chat", "not-a-cursor")
	f.Fuzz(func(t *testing.T, key, cursor string) {
		require := require.New(t)
		assert := assert.New(t)
		// JSON object keys and string values are Unicode; invalid byte encodings
		// belong to the separate raw parser rejection property below.
		key = strings.ToValidUTF8(key, "\uFFFD")
		cursor = strings.ToValidUTF8(cursor, "\uFFFD")
		oldKey := "old:" + key
		newKey := "new:" + key
		state := NewSyncState()
		state.SetChatCursor(oldKey, "2026-06-01T00:00:00Z")
		state.SetChannelDelta(oldKey, "old-token")
		raw, err := json.Marshal(map[string]any{
			"chats":               map[string]string{newKey: cursor},
			"channels":            map[string]string{newKey: "new-token"},
			"reset_chat_baseline": true,
		})
		require.NoError(err)
		checkpoint, err := LoadSyncState(string(raw))
		require.NoError(err)
		state.Merge(checkpoint)
		blob, err := state.Marshal()
		require.NoError(err)
		restored, err := LoadSyncState(blob)
		require.NoError(err)
		assert.Equal(map[string]string{newKey: cursor}, restored.Chats)
		assert.Equal(map[string]string{oldKey: "old-token", newKey: "new-token"}, restored.Channels)
	})
}

func FuzzLoadSyncStateRejectsMalformedJSON(f *testing.F) {
	f.Add([]byte("{"))
	f.Add([]byte("\xff"))
	f.Add([]byte("null"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		// Empty input deliberately means an initial sync state. Valid JSON may be
		// rejected by the typed decoder too; this property checks malformed syntax.
		if len(raw) == 0 || json.Valid(raw) {
			return
		}
		_, err := LoadSyncState(string(raw))
		require.Error(t, err)
	})
}
