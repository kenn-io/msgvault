package store_test

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPSourceOccurrencePinsOriginWithoutStoppingSource(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	const family = "msgvault.source_message_archived"
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	cfg := mcpStoreConfig()
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: family, SourceType: "gmail", Kinds: []string{"message"}})
	_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	now := time.Now().UTC()
	input := newMCPStoreSubscription(t, f, 71, now)
	input.Name = family
	input.ScopeKind = "source"
	input.ScopeID = f.Source.ID
	input.Arguments, err = json.Marshal(map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10), "source_ref": resource.Reference, "include_from_me": false, "include_reactions": true})
	require.NoError(err)
	require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	persist := func(nativeID string) int64 {
		t.Helper()
		id, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID(nativeID).Build(), BodyText: sql.NullString{String: "Synthetic source occurrence", Valid: true}})
		Require.NoError(t, err)
		return id
	}
	first := persist("origin-first")
	envelope := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic-source-origin"}`), nil
	}
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pending)
	require.Equal(first, pending.Event.MessageID)
	_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
	require.NoError(err)
	var original sql.NullString
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT source_conversation_id FROM conversations WHERE id=?`), f.ConvID).Scan(&original))
	// Exercise the native identity triggers, including an away-and-back update.
	// Archive content and the encompassing source remain present throughout.
	var originalID any
	if original.Valid {
		originalID = original.String
	}
	for _, nativeID := range []any{"replacement-origin", originalID} {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET source_conversation_id=? WHERE id=?`), nativeID, f.ConvID)
		require.NoError(err)
	}
	unchanged, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	assert.Equal(resource.Reference, unchanged.Reference)
	_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
	require.Error(err, "source validity cannot authorize a replaced origin")
	require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now), "origin replacement must be rechecked before receiver I/O")
	called := false
	err = f.Store.ReadMCPEventMessage(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, first, now, func(*sql.Tx) error { called = true; return nil })
	require.Error(err)
	assert.False(called)
	f.ConvID, err = f.Store.EnsureConversationWithType(f.Source.ID, "unaffected-origin", "email", "Synthetic unaffected conversation")
	require.NoError(err)
	second := persist("origin-second")
	next, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(time.Minute), envelope)
	require.NoError(err)
	require.NotNil(next)
	assert.Equal(second, next.Event.MessageID)
	current, err := f.Store.GetMCPSubscription(t.Context(), sub.ID)
	require.NoError(err)
	assert.Equal("active", current.State)
}

func TestNativeSlackConversationRetirementInvalidatesReceiptsAndPendingDeliveries(t *testing.T) {
	for _, retirement := range []string{"explicit", "complete_inventory", "read_denial"} {
		t.Run(retirement, func(t *testing.T) {
			require, assert := Require.New(t), Assert.New(t)
			f := storetest.New(t)
			var err error
			f.Source, err = f.Store.GetOrCreateSource("slack", "T01:UME")
			require.NoError(err)
			f.ConvID, err = f.Store.EnsureConversationWithType(f.Source.ID, "CAFFECTED", "channel", "Synthetic affected channel")
			require.NoError(err)
			other, err := f.Store.EnsureConversationWithType(f.Source.ID, "COTHER", "channel", "Synthetic other channel")
			require.NoError(err)
			grantSyntheticSourceFixture(t, f)
			source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
			require.NoError(err)
			inventory := store.NativeMessageScopeInventory{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "slack", NativeIdentity: "T01:UME", Kind: "slack_channel", Scopes: []store.NativeMessageScope{{ProviderID: "CAFFECTED", PermissionDomain: "channels"}, {ProviderID: "COTHER", PermissionDomain: "channels"}}}
			require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
			_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "slack", Kinds: []string{"message"}}, {Family: "msgvault.source_message_archived", SourceType: "slack", Kinds: []string{"message"}}}})
			require.NoError(err)
			now := time.Now().UTC()
			activate := func(input store.MCPSubscription) *store.MCPSubscription {
				t.Helper()
				sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
				Require.NoError(t, err)
				return sub
			}
			conversation := activate(newMCPStoreSubscription(t, f, 81, now))
			input := newMCPStoreSubscription(t, f, 82, now)
			input.Name, input.ScopeKind, input.ScopeID = "msgvault.source_message_archived", "source", f.Source.ID
			input.Arguments, err = json.Marshal(map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10), "source_ref": source.Reference})
			require.NoError(err)
			require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
			account := activate(input)
			persist := func(native string) int64 {
				t.Helper()
				id, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID(native).Build(), BodyText: sql.NullString{String: "Synthetic Slack body", Valid: true}})
				Require.NoError(t, err)
				return id
			}
			message := persist("CAFFECTED:1.000001")
			envelope := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"synthetic"}`), nil
			}
			convPending, err := f.Store.PrepareMCPDelivery(t.Context(), conversation.ID, conversation.Generation, now, envelope)
			require.NoError(err)
			require.NotNil(convPending)
			sourcePending, err := f.Store.PrepareMCPDelivery(t.Context(), account.ID, account.Generation, now, envelope)
			require.NoError(err)
			require.NotNil(sourcePending)
			require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), account.ID, account.Generation, sourcePending.Event.Seq, now))
			_, err = f.Store.GetMCPEvent(t.Context(), conversation.ID, convPending.Event.Seq, conversation.Principal, now)
			require.NoError(err)
			inventory.Scopes = []store.NativeMessageScope{{ProviderID: "COTHER", PermissionDomain: "channels"}}
			switch retirement {
			case "explicit":
				inventory.RetiredProviderIDs = []string{"CAFFECTED"}
				require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
			case "complete_inventory":
				inventory.Complete = true
				require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
			case "read_denial":
				pin, found, err := f.Store.LookupNativeMessageScope(t.Context(), f.Source.ID, source.Reference, "slack_channel", "CAFFECTED")
				require.NoError(err)
				require.True(found)
				require.NoError(f.Store.RejectNativeMessageScopeRead(t.Context(), store.NativeMessageScopeRead{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "slack", NativeIdentity: "T01:UME", Scopes: []store.NativeMessageScopeLookup{pin}}))
			}
			_, _, err = f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", f.ConvID, []string{"message"})
			require.Error(err, "a retired native channel cannot admit a conversation subscription")
			for _, sub := range []*store.MCPSubscription{conversation, account} {
				seq := convPending.Event.Seq
				if sub == account {
					seq = sourcePending.Event.Seq
				}
				require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, seq, now))
				_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, seq, sub.Principal, now)
				require.Error(err)
				called := false
				err = f.Store.ReadMCPEventMessage(t.Context(), sub.ID, seq, sub.Principal, message, now, func(*sql.Tx) error { called = true; return nil })
				require.Error(err)
				assert.False(called)
			}
			require.NoError(f.Store.CheckMCPSubscription(t.Context(), account.ID, account.Generation, now), "the source subscription remains valid")
			_, _, err = f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", other, []string{"message"})
			require.NoError(err)
			// A restored native channel admits new work without reviving old receipts.
			inventory.Complete, inventory.RetiredProviderIDs = false, nil
			inventory.Scopes = []store.NativeMessageScope{{ProviderID: "CAFFECTED", PermissionDomain: "channels"}}
			require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
			_, err = f.Store.GetMCPEvent(t.Context(), conversation.ID, convPending.Event.Seq, conversation.Principal, now)
			require.Error(err, "restoration cannot revive a retired receipt")
			require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), account.ID, account.Generation, sourcePending.Event.Seq, now))
			f.ConvID = other
			second := persist("COTHER:2.000001")
			next, err := f.Store.PrepareMCPDelivery(t.Context(), account.ID, account.Generation, now.Add(time.Second), envelope)
			require.NoError(err)
			require.NotNil(next)
			assert.Equal(second, next.Event.MessageID)
			require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), account.ID, account.Generation, next.Event.Seq, now.Add(time.Second)))
		})
	}
}
