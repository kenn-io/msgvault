package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func teamsEventScopeFixture(t *testing.T, kind string) (*storetest.Fixture, store.MCPEventsConfig) {
	t.Helper()
	require := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("teams", "owner@example.test")
	require.NoError(err)
	conv, err := f.Store.EnsureConversationWithType(source.ID, "synthetic-chat", kind, "Synthetic conversation")
	require.NoError(err)
	f.Source, f.ConvID = source, conv
	cfg := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "teams", Kinds: []string{"message"}},
		{Family: "msgvault.draft_changed", SourceType: "teams", Kinds: []string{"created", "updated", "deleted"}},
	}}
	_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	return f, cfg
}

func TestMCPTeamsChatOnlyAdmissionAndCapture(t *testing.T) {
	for _, kind := range []string{"direct_chat", "group_chat", "channel", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f, _ := teamsEventScopeFixture(t, kind)
			allowed := kind == "direct_chat" || kind == "group_chat"
			_, _, err := f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", f.ConvID, []string{"message"})
			if allowed {
				require.NoError(err)
			} else {
				require.Error(err)
			}
			live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
			_, err = live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build(), BodyText: sql.NullString{String: "Synthetic body", Valid: true}})
			require.NoError(err)
			var events int
			require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&events))
			if allowed {
				assert.Equal(1, events)
			} else {
				assert.Zero(events)
			}
			// Draft capabilities are independent of live provider message support.
			_, _, err = f.Store.ValidateMCPEventScope(t.Context(), "msgvault.draft_changed", "conversation", f.ConvID, []string{"created"})
			require.NoError(err)
			_, err = f.Store.CreateChatDraftContext(t.Context(), f.ConvID, 0, "Synthetic draft", func(string, string) error { return nil })
			require.NoError(err)
			require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.draft_changed'`).Scan(&events))
			assert.Equal(1, events)
		})
	}
}

func TestMCPTeamsScopeChangeRejectsPendingDeliveryAndReceipts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _ := teamsEventScopeFixture(t, "group_chat")
	now := time.Now().UTC()
	sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now})
	require.NoError(err)
	require.NotNil(sub)
	id, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build(), BodyText: sql.NullString{String: "Synthetic body", Valid: true}})
	require.NoError(err)
	envelope := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic"}`), nil
	}
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pending)
	seq := pending.Event.Seq
	_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, seq, sub.Principal, now)
	require.NoError(err)
	// A native classification update occurs after preparation but before the
	// worker preflight, and the stored pending envelope already exists.
	changed, err := f.Store.EnsureConversationWithType(f.Source.ID, "synthetic-chat", "channel", "Synthetic conversation")
	require.NoError(err)
	assert.Equal(f.ConvID, changed)
	require.Error(f.Store.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, now))
	_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, seq, sub.Principal, now)
	require.Error(err)
	called := false
	err = f.Store.ReadMCPEventMessage(t.Context(), sub.ID, seq, sub.Principal, id, now, func(*sql.Tx) error { called = true; return nil })
	require.Error(err)
	assert.False(called, "unauthorized receipt must not invoke content reader")
	retried, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(time.Second), envelope)
	require.NoError(err)
	assert.Nil(retried, "pending bytes cannot bypass current chat-only boundary")
	stopped, err := f.Store.GetMCPSubscription(t.Context(), sub.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("scope_removed", stopped.StopReason)
	assert.Empty(stopped.PendingEnvelope)
}

func TestMCPTeamsCaptureDisabledRetainsChatReceipts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, cfg := teamsEventScopeFixture(t, "direct_chat")
	now := time.Now().UTC()
	sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now})
	require.NoError(err)
	require.NotNil(sub)
	id, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build(), BodyText: sql.NullString{String: "Synthetic body", Valid: true}})
	require.NoError(err)
	delivery, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic"}`), nil
	})
	require.NoError(err)
	require.NotNil(delivery)
	require.NoError(f.Store.FinishMCPDelivery(t.Context(), sub.ID, sub.Generation, delivery.Event.Seq, now, 200, time.Time{}))
	cfg.Enabled = false
	cfg.Capabilities = nil
	_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	event, err := f.Store.GetMCPEvent(t.Context(), sub.ID, delivery.Event.Seq, sub.Principal, now)
	require.NoError(err)
	assert.Equal(id, event.MessageID)
	var body string
	require.NoError(f.Store.ReadMCPEventMessage(t.Context(), sub.ID, delivery.Event.Seq, sub.Principal, id, now, func(tx *sql.Tx) error {
		return tx.QueryRow(f.Store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body)
	}))
	assert.Equal("Synthetic body", body)
}
