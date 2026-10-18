package store_test

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestNativeSlackChannelRetirementRevokesOnlyItsConversation(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("slack", "T_SYNTH:U_OWNER")
	require.NoError(err)
	retiredConversation, err := f.Store.EnsureConversationWithType(source.ID, "C_RETIRED", "channel", "Synthetic retired channel")
	require.NoError(err)
	otherConversation, err := f.Store.EnsureConversationWithType(source.ID, "C_OTHER", "channel", "Synthetic other channel")
	require.NoError(err)
	sourceResource, err := f.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: sourceResource.Reference, SourceType: "slack", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
	inventory := store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: sourceResource.Reference, SourceType: "slack", NativeIdentity: source.Identifier, Kind: "slack_channel", Complete: true, Scopes: []store.NativeMessageScope{
		{ProviderID: "C_RETIRED", DisplayName: "Synthetic retired channel", PermissionDomain: "channels"},
		{ProviderID: "C_OTHER", DisplayName: "Synthetic other channel", PermissionDomain: "channels"},
	}}
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	cfg := mcpStoreConfig()
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "slack", Kinds: []string{"message"}})
	_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)

	newSubscription := func(id string, conversationID int64, now time.Time) *store.MCPSubscription {
		t.Helper()
		arguments, err := json.Marshal(map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10), "include_from_me": false})
		require.NoError(err)
		sub := &store.MCPSubscription{ID: id, Principal: cfg.Principal, Name: "msgvault.message_archived", Arguments: arguments, ScopeKind: "conversation", ScopeID: conversationID, SourceID: source.ID, CallbackURL: "https://receiver.example.net/events", SecretEnc: []byte("synthetic-encrypted-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour)}
		require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), sub))
		active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: *sub, Now: now})
		require.NoError(err)
		require.NotNil(active)
		return active
	}
	now := time.Now().UTC()
	retiredResource, err := f.Store.GetMCPResource(t.Context(), "conversation", retiredConversation)
	require.NoError(err)
	otherResource, err := f.Store.GetMCPResource(t.Context(), "conversation", otherConversation)
	require.NoError(err)
	retiredSub := newSubscription("sub_slack_retired", retiredConversation, now)
	otherSub := newSubscription("sub_slack_other", otherConversation, now)
	appendMessage := func(conversationID int64, id string) int64 {
		t.Helper()
		messageID, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: now}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: storetest.NewMessage(source.ID, conversationID).WithSourceMessageID(id).Build(), BodyText: sql.NullString{String: "Synthetic Slack message", Valid: true}})
		require.NoError(err)
		return messageID
	}
	retiredMessage := appendMessage(retiredConversation, "C_RETIRED:1.000001")
	otherMessage := appendMessage(otherConversation, "C_OTHER:1.000001")
	seqFor := func(messageID int64) int64 {
		t.Helper()
		var seq int64
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT seq FROM mcp_event_log WHERE family='msgvault.message_archived' AND message_id=?`), messageID).Scan(&seq))
		return seq
	}
	retiredSeq, otherSeq := seqFor(retiredMessage), seqFor(otherMessage)
	makePending := func(sub *store.MCPSubscription) {
		t.Helper()
		pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
			return event.Data, nil
		})
		require.NoError(err)
		require.NotNil(pending)
	}
	makePending(retiredSub)
	makePending(otherSub)
	_, _, err = f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", retiredConversation, []string{"message"})
	require.NoError(err)
	_, err = f.Store.GetMCPEvent(t.Context(), retiredSub.ID, retiredSeq, retiredSub.Principal, now)
	require.NoError(err)

	inventory.Complete = false
	inventory.Scopes = nil
	inventory.RetiredProviderIDs = []string{"C_RETIRED"}
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	_, _, err = f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", retiredConversation, []string{"message"})
	require.Error(err, "retired Slack channel cannot admit new conversation subscriptions")
	_, _, err = f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", otherConversation, []string{"message"})
	require.NoError(err, "retiring one channel leaves the other channel authorized")
	require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), retiredSub.ID, retiredSub.Generation, retiredSeq, now))
	require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), otherSub.ID, otherSub.Generation, otherSeq, now))
	_, err = f.Store.GetMCPEvent(t.Context(), retiredSub.ID, retiredSeq, retiredSub.Principal, now)
	require.Error(err, "retired channel receipt is no longer readable")
	_, err = f.Store.GetMCPEvent(t.Context(), otherSub.ID, otherSeq, otherSub.Principal, now)
	require.NoError(err, "unrelated channel receipt remains readable")
	_, err = f.Store.PrepareMCPDelivery(t.Context(), retiredSub.ID, retiredSub.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
		return event.Data, nil
	})
	require.NoError(err)
	stopped, err := f.Store.GetMCPSubscription(t.Context(), retiredSub.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("scope_removed", stopped.StopReason)

	inventory.RetiredProviderIDs = nil
	inventory.Scopes = []store.NativeMessageScope{{ProviderID: "C_RETIRED", DisplayName: "Synthetic restored channel", PermissionDomain: "channels"}}
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	restoredResource, err := f.Store.GetMCPResource(t.Context(), "conversation", retiredConversation)
	require.NoError(err)
	assert.NotEqual(retiredResource.Reference, restoredResource.Reference, "restoration creates a new conversation lifetime")
	_, _, err = f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", retiredConversation, []string{"message"})
	require.NoError(err, "the restored channel can accept a new subscription")
	require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), retiredSub.ID, retiredSub.Generation, retiredSeq, now), "restoration cannot revive the retired subscription")
	_, err = f.Store.GetMCPEvent(t.Context(), retiredSub.ID, retiredSeq, retiredSub.Principal, now)
	require.Error(err, "restoration cannot revive the retired receipt")
	newSubscription("sub_slack_restored", retiredConversation, now)
	assert.Equal(otherResource.Reference, mustMCPResource(t, f.Store, "conversation", otherConversation).Reference, "unrelated channel lifetime remains unchanged")
}

func mustMCPResource(t *testing.T, st *store.Store, kind string, id int64) store.MCPResource {
	t.Helper()
	resource, err := st.GetMCPResource(t.Context(), kind, id)
	Require.NoError(t, err)
	return resource
}
