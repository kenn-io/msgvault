package beeper

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The independent oracle decodes the complete object strictly, so only one
// unambiguous boolean isSender=false can admit a message.
func FuzzInboxIncomingRequiresExplicitFalse(f *testing.F) {
	for _, seed := range []string{`false`, `true`, `null`, `"false"`, `0`, `{}`, `[]`, ``, "\x00", `false,"isSender":true`, `false,"metadata":true`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, value []byte) {
		raw := append([]byte(`{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"opaque","timestamp":"2026-01-01T00:00:00Z","isSender":`), value...)
		raw = append(raw, '}')
		var decoded any
		valid := jsontext.Value(raw).IsValid() && json.Unmarshal(raw, &decoded) == nil
		object, isObject := decoded.(map[string]any)
		boolean, isBoolean := object["isSender"].(bool)
		want := valid && isObject && isBoolean && !boolean
		candidate, incoming, err := inboxIncomingCandidate(raw, inboxTestTarget())
		if want {
			require.NoError(t, err)
			require.True(t, incoming)
			assert.Equal(t, inboxCandidate{ID: "message-A", SortKey: "opaque"}, candidate)
		} else {
			assert.False(t, incoming)
		}
	})
}
