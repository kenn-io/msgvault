package store_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func activateMCPConversationSubscription(t *testing.T, f *storetest.Fixture, id int, conversationID int64, now time.Time) (store.MCPSubscription, *store.MCPSubscription) {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10), "include_from_me": false})
	Require.NoError(t, err)
	input := store.MCPSubscription{ID: fmt.Sprintf("sub_%064x", id), Principal: "owner:synthetic", Name: "msgvault.message_archived", Arguments: arguments, ScopeKind: "conversation", ScopeID: conversationID, SourceID: f.Source.ID, CallbackURL: fmt.Sprintf("https://receiver.example.net/hook/%d", id), SecretEnc: []byte("synthetic-encrypted-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(24 * time.Hour)}
	Require.NoError(t, f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	active, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	Require.NoError(t, err)
	Require.False(t, truncated)
	return input, active
}

func insertMCPConversationEvent(t *testing.T, f *storetest.Fixture, epoch, seq, conversationID int64, recorded time.Time) {
	t.Helper()
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (?,?,'msgvault.message_archived','message','conversation',?,?,?,?,?,FALSE,?,?,?)`), seq, epoch, conversationID, fmt.Sprintf("message:%d", seq), seq, conversationID, f.Source.ID, recorded.Format(time.RFC3339Nano), recorded.Format(time.RFC3339Nano), `{"kind":"message","from_me":false}`)
	Require.NoError(t, err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_clock SET head_seq=? WHERE singleton=1`), seq)
	Require.NoError(t, err)
}

// Retention removes events from other conversations without truncating a
// subscription whose own conversation stayed quiet, while a subscription that
// loses an undelivered event in its own conversation still stops.
func TestMCPEventsPruningOnlyStopsSubscriptionsThatLoseTheirOwnEvents(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	// Keep activation-time pruning out of the way; the explicit sweep below
	// uses the shorter window.
	cfg.Retention = 9 * 24 * time.Hour
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	busyConversation, err := f.Store.EnsureConversation(f.Source.ID, "synthetic-busy-thread", "Busy Thread")
	require.NoError(err)
	lossyConversation, err := f.Store.EnsureConversation(f.Source.ID, "synthetic-lossy-thread", "Lossy Thread")
	require.NoError(err)

	quietInput, quiet := activateMCPConversationSubscription(t, f, 1, f.ConvID, now)
	_, lossy := activateMCPConversationSubscription(t, f, 2, lossyConversation, now)
	old := now.Add(-8 * 24 * time.Hour)
	insertMCPConversationEvent(t, f, clock.Epoch, 1, busyConversation, old)
	insertMCPConversationEvent(t, f, clock.Epoch, 2, lossyConversation, old)

	require.NoError(f.Store.PruneMCPEvents(t.Context(), now, 7*24*time.Hour))

	kept, err := f.Store.GetMCPSubscription(t.Context(), quiet.ID)
	require.NoError(err)
	require.NotNil(kept)
	assert.Equal("active", kept.State)
	assert.Empty(kept.StopReason)
	assert.Equal(quiet.Generation, kept.Generation)
	assert.Equal(int64(2), kept.CursorSeq)

	delivery, err := f.Store.PrepareMCPDelivery(t.Context(), quiet.ID, quiet.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"unexpected"}`), nil
	})
	require.NoError(err)
	assert.Nil(delivery)
	quietInput.ExpiresAt = now.Add(24 * time.Hour)
	renewed, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: quietInput, ExpectedGeneration: kept.Generation, ExpectedState: kept.State, ExpectedSecretRevision: kept.SecretRevision, Now: now})
	require.NoError(err)
	assert.False(truncated, "a quiet conversation lost nothing to retention")
	assert.Equal("active", renewed.State)

	stopped, err := f.Store.GetMCPSubscription(t.Context(), lossy.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State)
	assert.Equal("retention", stopped.StopReason)
	assert.Equal(lossy.CursorSeq, stopped.CursorSeq)
}

// An expired subscription keeps its cursor through pruning. A later sweep no
// longer sees the deleted event, so it must not mistake the lost scope for a
// quiet one; renewal within grace reports the truncation.
func TestMCPEventsPruningKeepsExpiredSubscriptionLossVisible(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	cfg.Retention = 9 * 24 * time.Hour
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	input, active := activateMCPConversationSubscription(t, f, 1, f.ConvID, now)
	expiredAt := now.Add(25 * time.Hour)
	require.NoError(f.Store.ExpireMCPSubscriptions(t.Context(), expiredAt))
	expired, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(expired)
	require.Equal("expired", expired.State)
	insertMCPConversationEvent(t, f, clock.Epoch, 1, f.ConvID, now.Add(-8*24*time.Hour))

	sweep := expiredAt.Add(time.Minute)
	require.NoError(f.Store.PruneMCPEvents(t.Context(), sweep, 7*24*time.Hour))
	require.NoError(f.Store.PruneMCPEvents(t.Context(), sweep.Add(time.Minute), 7*24*time.Hour))

	input.ExpiresAt = sweep.Add(24 * time.Hour)
	_, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: expired.Generation, ExpectedState: expired.State, ExpectedSecretRevision: expired.SecretRevision, Now: sweep.Add(2 * time.Minute)})
	require.NoError(err)
	assert.True(truncated, "an event pruned while the subscription was expired is reported on renewal")
}
