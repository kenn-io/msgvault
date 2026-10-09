package mcpevents

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestVerificationRejectsConversationReusedDuringChallenge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, f, req := eventService(t)
	source, err := f.Store.GetOrCreateSource("slack", "synthetic-workspace")
	require.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "original-channel", "chat", "Original channel")
	require.NoError(err)
	_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: s.principal, Capabilities: []store.MCPEventCapability{{Family: "msgvault.draft_changed", SourceType: "slack", Kinds: []string{"created"}}}})
	require.NoError(err)
	req.Name = "msgvault.draft_changed"
	req.Arguments = map[string]any{"conversation_id": strconv.FormatInt(conversation, 10)}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var challenge map[string]any
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&challenge)) {
			return
		}
		close(entered)
		<-release
		assert.NoError(json.NewEncoder(rw).Encode(map[string]any{"challenge": challenge["challenge"]}))
	})
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() { _, err := s.Subscribe(t.Context(), s.principal, req); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		require.FailNow("verification challenge did not start")
	}
	require.NoError(f.Store.PurgeChannelContext(t.Context(), source.ID, "original-channel"))
	// Explicit reuse exercises PostgreSQL too; SQLite reuses this highest ID itself.
	override := ""
	if f.Store.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO conversations (id,source_id,source_conversation_id,conversation_type)`+override+` VALUES (?,?,'replacement-channel','chat')`), conversation, source.ID)
	require.NoError(err)
	unblock()
	select {
	case err := <-done:
		require.Error(err)
		var eventErr *Error
		require.ErrorAs(err, &eventErr)
		assert.Equal("unknown_scope", eventErr.Reason)
	case <-time.After(10 * time.Second):
		require.FailNow("verification challenge did not finish")
	}
	subs, err := f.Store.ListMCPSubscriptions(t.Context(), s.principal)
	require.NoError(err)
	assert.Empty(subs, "the original request must not subscribe to the replacement")
}
