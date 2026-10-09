package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func mcpDraftSubscription(t *testing.T, f *storetest.Fixture) (*store.MCPSubscription, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	input := store.MCPSubscription{ID: "sub_purge", Principal: "owner", Name: "msgvault.draft_changed", Arguments: []byte(`{}`), ScopeKind: "conversation", ScopeID: f.ConvID, CallbackURL: "https://receiver.example.net/events", SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour)}
	require.NoError(t, f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(t, err)
	return active, now
}

func TestMCPEventsChannelPurgeRejectsReusedConversation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := draftProducerFixture(t, "slack")
	active, now := mcpDraftSubscription(t, f)
	require.NoError(f.Store.PurgeChannelContext(t.Context(), f.Source.ID, "draft-conversation"))
	replacement, err := f.Store.EnsureConversationWithType(f.Source.ID, "replacement-channel", "chat", "Replacement channel")
	require.NoError(err)
	if !f.Store.IsPostgreSQL() {
		require.Equal(f.ConvID, replacement, "SQLite naturally reuses the purged highest conversation ID")
	}
	draft, err := f.Store.CreateChatDraftContext(t.Context(), replacement, 0, "Synthetic replacement draft", func(string, string) error { return nil })
	require.NoError(err)
	var delivered []byte
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
		delivered = event.Data
		return event.Data, nil
	})
	require.NoError(err)
	assert.Nil(pending, "purged subscription must not receive replacement draft %s: %s", draft.DraftID, delivered)
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("scope_removed", stopped.StopReason)
}

func TestMCPEventsPurgesEndPendingConversationSubscriptions(t *testing.T) {
	for _, path := range []string{"slack_channel", "discord_thread", "source", "serialized_source"} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/capture_%t", path, enabled), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				kind := "slack"
				channelID := "draft-conversation"
				if path == "discord_thread" {
					kind = "discord"
					channelID = "parent-channel"
				}
				f := draftProducerFixture(t, kind)
				active, now := mcpDraftSubscription(t, f)
				if path == "discord_thread" {
					_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE conversations SET metadata=? WHERE id=?`), `{"parent_channel_id":"parent-channel"}`, f.ConvID)
					require.NoError(err)
				}
				_, err := f.Store.CreateChatDraftContext(t.Context(), f.ConvID, 0, "Synthetic pending draft", func(string, string) error { return nil })
				require.NoError(err)
				pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
					return event.Data, nil
				})
				require.NoError(err)
				require.NotNil(pending)
				require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), active.ID, active.Generation, pending.Event.Seq, now))
				if !enabled {
					_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Principal: active.Principal})
					require.NoError(err)
				}
				switch path {
				case "source":
					err = f.Store.RemoveSource(f.Source.ID)
				case "serialized_source":
					_, _, err = f.Store.RemoveSourceSerialized(t.Context(), f.Source.ID)
				default:
					err = f.Store.PurgeChannelContext(t.Context(), f.Source.ID, channelID)
				}
				require.NoError(err)
				stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
				require.NoError(err)
				require.NotNil(stopped)
				assert.Equal("scope_removed", stopped.StopReason)
				assert.Equal("stopped", stopped.State)
				assert.Greater(stopped.Generation, active.Generation)
				assert.Zero(stopped.PendingSeq)
				assert.Zero(stopped.PendingGeneration)
				assert.Empty(stopped.PendingEnvelope)
				assert.Zero(stopped.AttemptCount)
				assert.Zero(stopped.NextAttemptAt)
				require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), active.ID, active.Generation, pending.Event.Seq, now))
				_, err = f.Store.GetMCPEvent(t.Context(), active.ID, pending.Event.Seq, active.Principal, now)
				require.Error(err)
				for _, table := range []string{"mcp_event_log", "mcp_event_conversation_refs"} {
					var count int
					require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count))
					assert.Zero(count)
				}
				require.NoError(f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, pending.Event.Seq, now, 204, time.Time{}))
				after, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
				require.NoError(err)
				assert.Equal(stopped, after, "stale completion must not revive a removed scope")
			})
		}
	}
}

func TestMCPEventsConversationReferenceRejectsPhysicalReuse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := draftProducerFixture(t, "slack")
	active, now := mcpDraftSubscription(t, f)
	_, err := f.Store.CreateChatDraftContext(t.Context(), f.ConvID, 0, "Synthetic original draft", func(string, string) error { return nil })
	require.NoError(err)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
		return event.Data, nil
	})
	require.NoError(err)
	require.NotNil(pending)
	// Exercise the reference guard independently of the purge's subscription cleanup.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM conversations WHERE id=?`), f.ConvID)
	require.NoError(err)
	override := ""
	if f.Store.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO conversations (id,source_id,source_conversation_id,conversation_type)`+override+` VALUES (?,?,'replacement-channel','chat')`), f.ConvID, f.Source.ID)
	require.NoError(err)
	replacement := *active
	replacement.ID = "sub_replacement"
	require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &replacement))
	assert.NotEqual(active.ConversationReferenceID, replacement.ConversationReferenceID)
	require.Error(f.Store.CheckMCPSubscription(t.Context(), active.ID, active.Generation, now))
	require.Error(f.Store.CheckMCPDeliveryOccurrence(t.Context(), active.ID, active.Generation, pending.Event.Seq, now))
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, pending.Event.Seq, active.Principal, now)
	require.Error(err)
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: *active, ExpectedGeneration: active.Generation, ExpectedState: active.State, ExpectedSecretRevision: active.SecretRevision, Now: now})
	require.Error(err)
	assert.Equal("unknown_scope", err.Error())
	delivery, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
		return event.Data, nil
	})
	require.NoError(err)
	assert.Nil(delivery)
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal("scope_removed", stopped.StopReason)
	assert.Empty(stopped.PendingEnvelope)
	current, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: replacement, Now: now})
	require.NoError(err)
	_, err = f.Store.CreateChatDraftContext(t.Context(), f.ConvID, 0, "Synthetic replacement draft", func(string, string) error { return nil })
	require.NoError(err)
	delivery, err = f.Store.PrepareMCPDelivery(t.Context(), current.ID, current.Generation, now, func(_ store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
		return event.Data, nil
	})
	require.NoError(err)
	require.NotNil(delivery, "a new subscription can deliver events for the replacement")
	require.NoError(f.Store.CheckMCPDeliveryOccurrence(t.Context(), current.ID, current.Generation, delivery.Event.Seq, now))
}
