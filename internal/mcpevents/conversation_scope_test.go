package mcpevents

import (
	"context"
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

func TestScopeRemovedSendsTerminatedEnvelope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, f, req := eventService(t)
	source, err := f.Store.GetOrCreateSource("slack", "synthetic-workspace")
	require.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "terminated-channel", "chat", "Terminated channel")
	require.NoError(err)
	_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: s.principal, Capabilities: []store.MCPEventCapability{{Family: "msgvault.draft_changed", SourceType: "slack", Kinds: []string{"created"}}}})
	require.NoError(err)
	req.Name = "msgvault.draft_changed"
	req.Arguments = map[string]any{"conversation_id": strconv.FormatInt(conversation, 10)}
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	receiver := installRecordingReceiver(t, s, func(int, []byte) int { return http.StatusInternalServerError })
	require.NoError(f.Store.PurgeChannelContext(t.Context(), source.ID, "terminated-channel"))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	posts := receiver.waitFor(t, 1, "the terminated envelope must be sent")
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row != nil && row.State == "stopped"
	}, 10*time.Second, 10*time.Millisecond)
	// A stopped row has no retry schedule, so waking the worker and seeing no
	// second POST is the proof that the failed envelope is not retried.
	s.Wake()
	require.Never(func() bool { return len(receiver.snapshot()) > 1 }, 500*time.Millisecond, 20*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("MCP Events supervisor did not stop")
	}

	assert.JSONEq(`{"type":"terminated","error":{"code":-32024,"message":"Forbidden","data":{"reason":"scope_removed"}}}`, string(posts[0].Body))
	assertSignedControl(t, result.ID, "terminated", posts[0])
	assert.Len(receiver.snapshot(), 1, "a failed terminated envelope is not retried")
	row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	assert.Equal("stopped", row.State)
	assert.Equal("scope_removed", row.StopReason)
}

func TestRefreshAfterScopeRemovedIsForbidden(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, f, req := eventService(t)
	source, err := f.Store.GetOrCreateSource("slack", "synthetic-workspace")
	require.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "refreshed-channel", "chat", "Refreshed channel")
	require.NoError(err)
	_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: s.principal, Capabilities: []store.MCPEventCapability{{Family: "msgvault.draft_changed", SourceType: "slack", Kinds: []string{"created"}}}})
	require.NoError(err)
	req.Name = "msgvault.draft_changed"
	req.Arguments = map[string]any{"conversation_id": strconv.FormatInt(conversation, 10)}
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	require.NoError(f.Store.PurgeChannelContext(t.Context(), source.ID, "refreshed-channel"))

	_, err = s.Subscribe(t.Context(), s.principal, req)
	var eventErr *Error
	require.ErrorAs(err, &eventErr)
	assert.Equal(&Error{Code: CodeForbidden, Reason: "subscription_unavailable"}, eventErr)

	req.Delivery.URL = "https://other.example.net/events"
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.ErrorAs(err, &eventErr)
	assert.Equal(&Error{Code: CodeInvalidParams, Reason: "unknown_scope"}, eventErr, "a new key for the deleted scope is an unknown scope")
}
