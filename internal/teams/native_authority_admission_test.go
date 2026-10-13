package teams

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Local helpers keep test failures bound to their test.
	Require "github.com/stretchr/testify/require" //nolint:importas // Local helpers keep test failures bound to their test.
	"go.kenn.io/msgvault/internal/store"
	"testing"
)

// A working archive connection without saved grant/account evidence is not
// current Events authority. Removing the native guard would reopen admission.
func TestNativeTeamsUnknownAuthorityCannotSubscribe(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f, imp, opts := newNativeTeamsEventFeed(t)
	imp.client.grantedScopes = nil
	f.set(nativeTeamsEventMessage("historical", "2026-01-01T00:00:00Z", "Synthetic archived message", "sender@example.test"))
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	_, err = imp.store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "teams", Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: "teams", Kinds: []string{"message"}},
	}})
	require.NoError(err)
	var conversationID int64
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), sum.SourceID, "event-chat").Scan(&conversationID))
	for _, scope := range []struct {
		family, kind string
		id           int64
	}{
		{"msgvault.message_archived", "conversation", conversationID},
		{"msgvault.source_message_archived", "source", sum.SourceID},
	} {
		_, _, err := imp.store.ValidateMCPEventScope(t.Context(), scope.family, scope.kind, scope.id, []string{"message"})
		require.EqualError(err, "native_authority_unavailable")
	}
	var messages int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages, "archive use is independent of unknown native Events authority")
}
