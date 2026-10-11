package beeper

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// The independent oracle decodes the field into an interface, so missing,
// null, strings, objects and numbers cannot inherit a bool's zero value.
func FuzzInboxIncomingRequiresExplicitFalse(f *testing.F) {
	for _, seed := range []string{`false`, `true`, `null`, `"false"`, `0`, `{}`, `[]`, ``, "\x00", `false,"isSender":true`, `false,"metadata":true`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, value []byte) {
		var decoded any
		valid := json.Unmarshal(value, &decoded) == nil
		boolean, isBoolean := decoded.(bool)
		want := valid && isBoolean && !boolean
		raw := append([]byte(`{"id":"message-A","chatID":"!chat:example.invalid","accountID":"account-example","sortKey":"opaque","timestamp":"2026-01-01T00:00:00Z","isSender":`), value...)
		raw = append(raw, '}')
		candidate, err := inboxIncomingCandidate(raw, inboxTestTarget())
		if want {
			require.NoError(t, err)
			require.NotNil(t, candidate)
			assert.Equal(t, inboxCandidate{ID: "message-A", SortKey: "opaque"}, *candidate)
		} else {
			assert.Nil(t, candidate)
		}
	})
}

// Every Provider5 mutation entry point must remain transport-free, across
// both shape-valid and hostile requests. The real Client uses a recording peer.
func FuzzInboxMutationNeverDispatches(f *testing.F) {
	for _, op := range []string{"archive", "unarchive", "set-read", "set-unread", "tags", "move", "create-folder", "", "unknown"} {
		f.Add(op, "!chat:example.invalid", int64(11), true)
	}
	peer := &inboxHTTPPeer{info: inboxTestInfo, chat: inboxTestChat, page: inboxTestPage}
	provider := NewInboxProvider(peer.client(f), inboxTestSource())
	f.Fuzz(func(t *testing.T, operation, id string, sourceID int64, dryRun bool) {
		target := inboxTestTarget()
		target.ProviderID = id
		target.SourceID = sourceID
		request := inboxcontrol.Request{Operation: inboxcontrol.Operation(operation), Target: &target, DryRun: dryRun}
		before := inboxcontrol.State{Target: target, Inbox: inboxBool(false), Read: inboxBool(false)}
		_, previewErr := provider.Preview(t.Context(), request, before)
		_, dispatchErr := provider.Dispatch(t.Context(), request, before)
		assert.ErrorIs(t, previewErr, inboxcontrol.ErrUnavailable)
		assert.ErrorIs(t, dispatchErr, inboxcontrol.ErrUnavailable)
		assert.Empty(t, peer.calls())
	})
}
