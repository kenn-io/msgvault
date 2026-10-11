package store_test

import (
	"database/sql"
	"fmt"
	"go.kenn.io/msgvault/internal/store"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Nested scopes use assertion objects.
	Require "github.com/stretchr/testify/require" //nolint:importas // Nested scopes use assertion objects.
)

func TestMCPEventsLegacyAdoptionRejectsContradictoryMessageBinding(t *testing.T) {
	for _, wrongSource := range []bool{true, false} {
		t.Run(map[bool]string{true: "source", false: "conversation"}[wrongSource], func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st := openMCPEventsLegacyFixture(t)
			rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
			require.NoError(err)
			require.Len(rows, 1)
			before := rows[0]
			var deliveredID, pendingID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_id FROM mcp_event_log WHERE seq=?`), before.CursorSeq).Scan(&deliveredID))
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_id FROM mcp_event_log WHERE seq=?`), before.PendingSeq).Scan(&pendingID))
			otherSource := before.SourceID
			if wrongSource {
				source, err := st.GetOrCreateSource("gmail", "foreign-synthetic@example.com")
				require.NoError(err)
				otherSource = source.ID
			}
			otherConversation, err := st.EnsureConversation(otherSource, "foreign-thread", "Foreign synthetic thread")
			require.NoError(err)
			// Preserve the old live reference sequence while contradicting the
			// archived account/scope association before the resource cutover.
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_id=?,conversation_id=? WHERE id=?`), otherSource, otherConversation, deliveredID)
			require.NoError(err)
			require.NoError(st.InitSchemaContext(t.Context()))
			cfg := legacyMCPEventsConfig(before.Principal, true)
			clock, err := st.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			assert.Equal(before.CursorEpoch, clock.Epoch)
			grantSyntheticSourceAuthority(t, st, before.SourceID)
			for attempt := range 2 {
				called := false
				err := st.ReadMCPEventMessage(t.Context(), before.ID, before.CursorSeq, before.Principal, deliveredID, before.UpdatedAt, func(*sql.Tx) error {
					called = true
					return nil
				})
				require.Error(err, "contradictory cutover identity must not authorize a body read, attempt %d", attempt)
				assert.False(called)
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_id=?,conversation_id=? WHERE id=?`), before.SourceID, before.ScopeID, deliveredID)
				require.NoError(err)
				require.NoError(st.InitSchemaContext(t.Context()))
				_, err = st.ConfigureMCPEvents(t.Context(), cfg)
				require.NoError(err)
			}
			// The untouched pending message remains valid; denying one bad old
			// association must not silently discard all retained authority.
			called := false
			require.NoError(st.ReadMCPEventMessage(t.Context(), before.ID, before.PendingSeq, before.Principal, pendingID, before.UpdatedAt, func(tx *sql.Tx) error {
				called = true
				var body string
				return tx.QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), pendingID).Scan(&body)
			}))
			assert.True(called)
			after, err := st.GetMCPSubscription(t.Context(), before.ID)
			require.NoError(err)
			require.NotNil(after)
			assert.Equal(before.PendingEnvelope, after.PendingEnvelope)
			assert.Equal("active", after.State)
		})
	}
}

func TestMCPEventsLegacyResourceLifetimeCannotRevive(t *testing.T) {
	for _, kind := range []string{"source", "conversation"} {
		t.Run(kind, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st := openMCPEventsLegacyFixture(t)
			rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
			require.NoError(err)
			require.Len(rows, 1)
			before := rows[0]
			require.NoError(st.InitSchemaContext(t.Context()))
			cfg := legacyMCPEventsConfig(before.Principal, true)
			_, err = st.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			grantSyntheticSourceAuthority(t, st, before.SourceID)
			now := before.UpdatedAt
			require.NoError(st.CheckMCPSubscription(t.Context(), before.ID, before.Generation, now))
			var original string
			table, column, id := "sources", "identifier", before.SourceID
			if kind == "conversation" {
				table, column, id = "conversations", "source_conversation_id", before.ScopeID
			}
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT "+column+" FROM "+table+" WHERE id=?"), id).Scan(&original))
			for _, value := range []string{"synthetic-rebound-identity", original} {
				_, err = st.DB().Exec(st.Rebind("UPDATE "+table+" SET "+column+"=? WHERE id=?"), value, id)
				require.NoError(err)
			}
			require.NoError(st.InitSchemaContext(t.Context()))
			_, err = st.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			require.Error(st.CheckMCPSubscription(t.Context(), before.ID, before.Generation, now))
			_, err = st.GetMCPEvent(t.Context(), before.ID, before.CursorSeq, before.Principal, now)
			require.Error(err)
			candidate := before
			candidate.ExpiresAt = now.Add(23 * time.Hour)
			_, _, err = st.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: candidate, ExpectedGeneration: before.Generation, ExpectedState: before.State, ExpectedSecretRevision: before.SecretRevision, Now: now})
			require.Error(err)
			called := false
			pending, err := st.PrepareMCPDelivery(t.Context(), before.ID, before.Generation, before.NextAttemptAt, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) { called = true; return []byte("{}"), nil })
			require.NoError(err)
			// Keep a failure from dumping a pending envelope or encrypted secret.
			deliveryReady := pending != nil
			assert.False(deliveryReady, "invalid lifetime must not prepare a delivery")
			assert.False(called)
			after, err := st.GetMCPSubscription(t.Context(), before.ID)
			require.NoError(err)
			require.NotNil(after)
			assert.Equal("scope_removed", after.StopReason)
		})
	}
}

func TestMCPEventsNewLifetimeCannotReplayPreviousResource(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := openMCPEventsLegacyFixture(t)
	rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(rows, 1)
	before := rows[0]
	require.NoError(st.InitSchemaContext(t.Context()))
	_, err = st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig(before.Principal, true))
	require.NoError(err)
	grantSyntheticSourceAuthority(t, st, before.SourceID)
	_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET identifier='replacement-synthetic@example.com' WHERE id=?`), before.SourceID)
	require.NoError(err)
	grantSyntheticSourceAuthority(t, st, before.SourceID)
	now := before.UpdatedAt
	candidate := before
	candidate.ID = fmt.Sprintf("sub_%064x", 994)
	candidate.ExpiresAt = now.Add(23 * time.Hour)
	require.NoError(st.BindMCPSubscriptionScope(t.Context(), &candidate))
	active, _, err := st.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: candidate, Replay: true, ReplayEpoch: before.CursorEpoch, Now: now})
	require.NoError(err)
	called := false
	pending, err := st.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) { called = true; return []byte("{}"), nil })
	require.NoError(err)
	deliveryReady := pending != nil
	assert.False(deliveryReady, "replay must not deliver a previous resource's occurrences")
	assert.False(called)
	messageID, err := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: now}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: before.SourceID, ConversationID: before.ScopeID, SourceMessageID: "new-resource-synthetic-message", MessageType: "email"}})
	require.NoError(err)
	pending, err = st.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) { return []byte("{}"), nil })
	require.NoError(err)
	require.NotNil(pending)
	assert.Equal(messageID, pending.Event.MessageID)
}
