package discord

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// Filtering happens before catalog reconciliation. Retained thread identities
// must still lose their authority, including when the bounded catalog omits them.
func TestNativeDiscordThreadExclusionRevokesPendingAndReceipts(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted=%v", omitted), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f, imp, opts := newNativeDiscordInboxFeed(t)
			f.threads = append(f.threads, Channel{ID: "401", GuildID: "200", Type: channelTypePublicThread, ParentID: "300", Name: "Synthetic sibling"})
			now, err := TimestampFromSnowflake("1500000000000000000")
			require.NoError(err)
			imp.now = func() time.Time { return now.Add(time.Hour) }
			f.messages["400"] = []Message{nativeDiscordInboxMessage("1500000000000000000", "400", "Synthetic history")}
			first, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			page, err := imp.store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: first.SourceID})
			require.NoError(err)
			resources := map[string]store.MCPResource{}
			for _, resource := range page.Resources {
				resources[resource.ProviderID] = resource
			}
			require.Contains(resources, "400")
			require.Contains(resources, "401")
			var conversationID int64
			require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id='400'`), first.SourceID).Scan(&conversationID))
			conversation, err := imp.store.GetMCPResource(t.Context(), "conversation", conversationID)
			require.NoError(err)
			clock := time.Now().UTC()
			var subscriptions []*store.MCPSubscription
			for i, scope := range []struct {
				family, kind string
				id           int64
			}{
				{"msgvault.message_archived", "conversation", conversationID},
				{"msgvault.source_message_archived", "source", first.SourceID},
				{"msgvault.inbox_message_archived", "inbox", resources["300"].ID},
				{"msgvault.inbox_message_archived", "inbox", resources["400"].ID},
			} {
				sub := store.MCPSubscription{ID: fmt.Sprintf("sub_%064x", i+1), Principal: "synthetic-owner", Name: scope.family, Arguments: []byte(`{}`), ScopeKind: scope.kind, ScopeID: scope.id, SourceID: first.SourceID, CallbackURL: "https://receiver.example.net/events", SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: clock.Add(time.Hour)}
				require.NoError(imp.store.BindMCPSubscriptionScope(t.Context(), &sub))
				active, _, err := imp.store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: sub, Now: clock})
				require.NoError(err)
				require.NotNil(active)
				subscriptions = append(subscriptions, active)
			}
			envelope := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"synthetic"}`), nil
			}
			delivered, pending := map[string]*store.MCPDelivery{}, map[string]*store.MCPDelivery{}
			for n := 1; n <= 2; n++ {
				f.mu.Lock()
				f.messages["400"] = append(f.messages["400"], nativeDiscordInboxMessage(strconv.FormatInt(1500000000000000000+int64(n), 10), "400", "Synthetic live message"))
				f.mu.Unlock()
				_, err = imp.Import(t.Context(), opts)
				require.NoError(err)
				for _, sub := range subscriptions {
					delivery, err := imp.store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, clock, envelope)
					require.NoError(err)
					require.NotNil(delivery)
					_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, delivery.Event.Seq, sub.Principal, clock)
					require.NoError(err)
					if n == 1 {
						_, err = imp.store.FinishMCPDelivery(t.Context(), sub.ID, sub.Generation, delivery.Event.Seq, clock, http.StatusNoContent, time.Time{})
						require.NoError(err)
						delivered[sub.ID] = delivery
					} else {
						pending[sub.ID] = delivery
					}
				}
			}
			opts.GuildConfig.Exclude = []string{"400"}
			if omitted {
				f.mu.Lock()
				f.threads = nil
				f.mu.Unlock()
			}
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			_, err = imp.store.GetMCPResource(t.Context(), "inbox", resources["400"].ID)
			require.Error(err, "the excluded retained thread must lose inbox authority")
			currentConversation, err := imp.store.GetMCPResource(t.Context(), "conversation", conversationID)
			require.NoError(err)
			assert.NotEqual(conversation.Reference, currentConversation.Reference, "exclusion retires the conversation origin")
			_, _, err = imp.store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", conversationID, []string{"message"})
			require.Error(err, "the archived thread cannot authorize a fresh subscription after exclusion")
			for _, providerID := range []string{"300", "301", "401"} {
				current, err := imp.store.GetMCPResource(t.Context(), "inbox", resources[providerID].ID)
				require.NoError(err)
				assert.Equal(resources[providerID].Reference, current.Reference, "independent parent and sibling authority is preserved")
			}
			for _, sub := range subscriptions {
				require.Error(imp.store.CheckMCPDeliveryOccurrence(t.Context(), sub.ID, sub.Generation, pending[sub.ID].Event.Seq, clock), "prepared bytes must fail the delivery preflight")
				for _, delivery := range []*store.MCPDelivery{delivered[sub.ID], pending[sub.ID]} {
					_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, delivery.Event.Seq, sub.Principal, clock)
					require.Error(err, "exclusion revokes delivered and pending receipts for every family")
					called := false
					err = imp.store.ReadMCPEventMessage(t.Context(), sub.ID, delivery.Event.Seq, sub.Principal, delivery.Event.MessageID, clock, func(*sql.Tx) error { called = true; return nil })
					require.Error(err)
					assert.False(called, "an excluded thread receipt cannot invoke the content reader")
				}
				retried, err := imp.store.PrepareMCPDelivery(t.Context(), sub.ID, sub.Generation, clock.Add(time.Second), envelope)
				require.NoError(err)
				current, err := imp.store.GetMCPSubscription(t.Context(), sub.ID)
				require.NoError(err)
				require.NotNil(current)
				assert.Empty(current.PendingEnvelope)
				if sub.ScopeKind == "source" || sub.ScopeID == resources["300"].ID && sub.ScopeKind == "inbox" {
					assert.Equal("active", current.State, "excluding one thread preserves broader subscriptions")
					assert.Nil(retried, "broader subscriptions have no pending delivery after the excluded thread")
				} else {
					assert.Equal("scope_removed", current.StopReason)
					require.NotNil(retried, "removed subscriptions return a termination control, not pending event bytes")
					require.NotNil(retried.Control)
					assert.Equal("terminated", retried.Control.Type)
					assert.Equal("scope_removed", retried.Control.Reason)
				}
			}
			opts.GuildConfig.Exclude = nil
			f.mu.Lock()
			f.threads = []Channel{{ID: "400", GuildID: "200", Type: channelTypePublicThread, ParentID: "300", Name: "Restored synthetic thread"}}
			f.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			for _, sub := range subscriptions {
				_, err = imp.store.GetMCPEvent(t.Context(), sub.ID, delivered[sub.ID].Event.Seq, sub.Principal, clock)
				require.Error(err, "restoration cannot revive the old occurrence origin")
			}
		})
	}
}
