package store_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"strconv"
	"testing"
	"time"
)

func TestMCPSourceMessageFiltersUseNativeAttributionAndReactions(t *testing.T) {
	for _, own := range []bool{false, true} {
		for _, reactions := range []bool{false, true} {
			t.Run(fmt.Sprintf("own_%v_reactions_%v", own, reactions), func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				const family = "msgvault.source_message_archived"
				f, target := reactionProducerFixture(t)
				grantSyntheticSourceFixture(t, f)
				cfg := mcpStoreConfig()
				cfg.Capabilities = []store.MCPEventCapability{
					{Family: "msgvault.message_archived", SourceType: "beeper", Kinds: []string{"message", "reaction"}},
					{Family: family, SourceType: "beeper", Kinds: []string{"message", "reaction"}},
				}
				_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
				require.NoError(err)
				owner := f.EnsureParticipant("owner@example.net", "Synthetic owner", "example.net")
				peer := f.EnsureParticipant("peer@example.net", "Synthetic peer", "example.net")
				require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
				resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
				require.NoError(err)
				var providerID string
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT source_conversation_id FROM conversations WHERE id=?`), f.ConvID).Scan(&providerID))
				require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: "beeper", NativeIdentity: resource.ProviderID, Kind: "beeper_member", ParentProviderID: resource.ProviderID, Scopes: []store.NativeMessageScope{{ProviderID: providerID, DisplayName: "Synthetic reaction chat"}}}))
				now := time.Now().UTC()
				input := newMCPStoreSubscription(t, f, 73, now)
				input.Name = family
				input.ScopeKind = "source"
				input.ScopeID = f.Source.ID
				input.Arguments, err = json.Marshal(map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10), "source_ref": resource.Reference, "include_from_me": own, "include_reactions": reactions})
				require.NoError(err)
				require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
				sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
				require.NoError(err)
				live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
				for i, sender := range []int64{owner, peer} {
					msg := f.NewMessage().WithSourceMessageID(fmt.Sprintf("filter-message-%d", i)).Build()
					msg.SenderID = sql.NullInt64{Int64: sender, Valid: true}
					_, err = live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: msg, BodyText: sql.NullString{String: "Synthetic filter body", Valid: true}})
					require.NoError(err)
				}
				require.NoError(live.ReplaceReactions(target, []store.ReactionRef{{ParticipantID: owner, Type: "emoji", Value: "heart"}, {ParticipantID: peer, Type: "emoji", Value: "thumbs-up"}}))
				var captured int
				require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family=? AND kind='reaction'`), family).Scan(&captured))
				assert.Equal(2, captured, "native reactions are separate source occurrences")
				var gotMessages, gotReactions, gotOwn int
				for {
					pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
						return []byte(`{"eventId":"synthetic-filter"}`), nil
					})
					require.NoError(err)
					if pending == nil {
						break
					}
					switch pending.Event.Kind {
					case "message":
						gotMessages++
					case "reaction":
						gotReactions++
					}
					if pending.Event.FromMe {
						gotOwn++
					}
					require.NoError(f.Store.FinishMCPDelivery(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now, 204, time.Time{}))
				}
				wantMessages, wantReactions, wantOwn := 1, 0, 0
				if own {
					wantMessages = 2
					wantOwn = 1
				}
				if reactions {
					wantReactions = 1
					if own {
						wantReactions = 2
						wantOwn = 2
					}
				}
				assert.Equal(wantMessages, gotMessages)
				assert.Equal(wantReactions, gotReactions)
				assert.Equal(wantOwn, gotOwn, "source family obeys actual message/reactor attribution")
			})
		}
	}
}

func TestMCPSourceTeamsOriginRevocationKeepsSourceActive(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	const family = "msgvault.source_message_archived"
	f, cfg := teamsEventScopeFixture(t, "group_chat")
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: family, SourceType: "teams", Kinds: []string{"message"}})
	_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	now := time.Now().UTC()
	input := newMCPStoreSubscription(t, f, 74, now)
	input.Name = family
	input.ScopeKind = "source"
	input.ScopeID = f.Source.ID
	input.Arguments, err = json.Marshal(map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10), "source_ref": resource.Reference, "include_from_me": false, "include_reactions": true})
	require.NoError(err)
	require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	persist := func(nativeID string) int64 {
		t.Helper()
		id, err := live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID(nativeID).Build(), BodyText: sql.NullString{String: "Synthetic Teams source", Valid: true}})
		Require.NoError(t, err)
		return id
	}
	first := persist("teams-source-first")
	envelope := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic-teams-source"}`), nil
	}
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pending)
	assert.Equal(first, pending.Event.MessageID)
	_, err = f.Store.EnsureConversationWithType(f.Source.ID, "synthetic-chat", "channel", "Synthetic conversation")
	require.NoError(err)
	_, err = f.Store.GetMCPEvent(t.Context(), sub.ID, pending.Event.Seq, sub.Principal, now)
	require.Error(err, "source cannot bypass native chat-only authority")
	require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending.Event.Seq, now))
	f.ConvID, err = f.Store.EnsureConversationWithType(f.Source.ID, "unaffected-teams-chat", "direct_chat", "Synthetic unaffected chat")
	require.NoError(err)
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), store.NativeMessageScopeInventory{
		SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: "teams", NativeIdentity: f.Source.Identifier, Kind: "teams_chat",
		Scopes: []store.NativeMessageScope{{ProviderID: "unaffected-teams-chat"}},
	}))
	second := persist("teams-source-second")
	next, err := f.Store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, now.Add(time.Minute), envelope)
	require.NoError(err)
	require.NotNil(next)
	assert.Equal(second, next.Event.MessageID)
	current, err := f.Store.GetMCPSubscription(t.Context(), sub.ID)
	require.NoError(err)
	assert.Equal("active", current.State)
}

func reactionProducerFixture(t *testing.T) (*storetest.Fixture, int64) {
	t.Helper()
	require := Require.New(t)

	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("beeper", "synthetic-beeper-account")
	require.NoError(err)
	conv, err := f.Store.EnsureConversationWithType(source.ID, "reaction-chat", "chat", "Synthetic reaction chat")
	require.NoError(err)
	f.Source, f.ConvID = source, conv
	sender := f.EnsureParticipant("sender@example.test", "Synthetic sender", "example.test")
	metadata := sql.NullString{String: `{"provider":"synthetic"}`, Valid: true}
	id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, ConversationID: conv, SourceMessageID: "old-target", MessageType: "beeper",
			SenderID: sql.NullInt64{Int64: sender, Valid: true},
			SentAt:   sql.NullTime{Time: producerObservedAt.AddDate(0, 0, -30), Valid: true},
		},
		Metadata:   &metadata,
		BodyText:   sql.NullString{String: "Fully archived synthetic reaction target", Valid: true},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{"sender@example.test"}}},
	})
	require.NoError(err)
	return f, id
}
