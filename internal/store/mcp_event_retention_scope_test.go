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
	grantSyntheticSourceFixture(t, f)
	arguments, err := json.Marshal(map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10), "include_from_me": false})
	Require.NoError(t, err)
	input := store.MCPSubscription{ID: fmt.Sprintf("sub_%064x", id), Principal: "owner:synthetic", Name: "msgvault.message_archived", Arguments: arguments, ScopeKind: "conversation", ScopeID: conversationID, SourceID: f.Source.ID, CallbackURL: fmt.Sprintf("https://receiver.example.net/hook/%d", id), SecretEnc: []byte("synthetic-encrypted-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(24 * time.Hour)}
	Require.NoError(t, f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	active, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	Require.NoError(t, err)
	Require.False(t, truncated)
	return input, active
}

// Retention removes events from other conversations without truncating a
// subscription whose own conversation stayed quiet, while a subscription that
// loses an undelivered event in its own conversation stays active with a
// pending gap report.
func TestMCPEventsPruningOnlyReportsGapsForSubscriptionsThatLoseTheirOwnEvents(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
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
	f.InsertMCPConversationEvent(clock.Epoch, 1, busyConversation, old)
	f.InsertMCPConversationEvent(clock.Epoch, 2, lossyConversation, old)

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
	assert.Equal("active", stopped.State)
	assert.Empty(stopped.StopReason)
	assert.Equal("gap_pending", stopped.LastOutcome)
	assert.Equal(lossy.CursorSeq, stopped.CursorSeq)
	// A second sweep cannot see the deleted event, but the marker keeps the
	// loss from reading as a quiet scope.
	require.NoError(f.Store.PruneMCPEvents(t.Context(), now.Add(time.Minute), 7*24*time.Hour))
	again, err := f.Store.GetMCPSubscription(t.Context(), lossy.ID)
	require.NoError(err)
	assert.Equal(lossy.CursorSeq, again.CursorSeq)
	gap, err := f.Store.PrepareMCPDelivery(t.Context(), lossy.ID, lossy.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"unexpected"}`), nil
	})
	require.NoError(err)
	require.NotNil(gap)
	require.NotNil(gap.Control)
	assert.Equal("gap", gap.Control.Type)
	assert.Equal(clock.Epoch, gap.Control.Cursor.Epoch)
	assert.Equal(int64(2), gap.Control.Cursor.Seq)
}

// An expired subscription keeps its cursor through pruning. A later sweep no
// longer sees the deleted event, so it must not mistake the lost scope for a
// quiet one; renewal within grace reports the truncation.
func TestMCPEventsPruningKeepsExpiredSubscriptionLossVisible(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
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
	f.InsertMCPConversationEvent(clock.Epoch, 1, f.ConvID, now.Add(-8*24*time.Hour))

	sweep := expiredAt.Add(time.Minute)
	require.NoError(f.Store.PruneMCPEvents(t.Context(), sweep, 7*24*time.Hour))
	require.NoError(f.Store.PruneMCPEvents(t.Context(), sweep.Add(time.Minute), 7*24*time.Hour))

	input.ExpiresAt = sweep.Add(24 * time.Hour)
	_, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: expired.Generation, ExpectedState: expired.State, ExpectedSecretRevision: expired.SecretRevision, Now: sweep.Add(2 * time.Minute)})
	require.NoError(err)
	assert.True(truncated, "an event pruned while the subscription was expired is reported on renewal")
}

// An owner change revokes retained subscriptions even while Events stay off,
// so restoring the old owner later cannot renew them. A process that does not
// know the owner revokes nothing.
func TestMCPEventsDisabledOwnerChangeRevokesRetainedSubscriptions(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	now := time.Now().UTC()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	_, active := activateMCPConversationSubscription(t, f, 1, f.ConvID, now)
	disabled := mcpStoreConfig()
	disabled.Enabled = false
	_, err = f.Store.ConfigureMCPEvents(t.Context(), disabled)
	require.NoError(err)

	_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{})
	require.NoError(err)
	unknownOwner, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(unknownOwner)
	assert.Equal("active", unknownOwner.State, "an unknown owner is not an owner change")
	assert.Empty(unknownOwner.StopReason)

	disabled.Principal = "owner:synthetic-rotated"
	_, err = f.Store.ConfigureMCPEvents(t.Context(), disabled)
	require.NoError(err)
	rotated, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(rotated)
	assert.Equal("principal_revoked", rotated.StopReason)
}

// A refresh reports the loss and places the cursor, so the pending-gap marker
// is spent. A later sweep over a quiet scope must advance the cursor as usual
// instead of treating the row as still lossy.
func TestMCPEventsRefreshClearsGapPendingMarker(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	cfg.Retention = 9 * 24 * time.Hour
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	busy, err := f.Store.EnsureConversation(f.Source.ID, "synthetic-busy-thread", "Busy Thread")
	require.NoError(err)
	input, active := activateMCPConversationSubscription(t, f, 1, f.ConvID, now)
	old := now.Add(-8 * 24 * time.Hour)
	f.InsertMCPConversationEvent(clock.Epoch, 1, f.ConvID, old)
	require.NoError(f.Store.PruneMCPEvents(t.Context(), now, 7*24*time.Hour))
	marked, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.Equal("gap_pending", marked.LastOutcome)

	input.ExpiresAt = now.Add(24 * time.Hour)
	refreshed, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: marked.Generation, ExpectedState: marked.State, ExpectedSecretRevision: marked.SecretRevision, Now: now})
	require.NoError(err)
	assert.True(truncated)
	assert.Equal(int64(1), refreshed.CursorSeq)
	assert.NotEqual("gap_pending", refreshed.LastOutcome)

	// Another conversation's old event is pruned; this scope stays quiet.
	f.InsertMCPConversationEvent(clock.Epoch, 2, busy, old)
	require.NoError(f.Store.PruneMCPEvents(t.Context(), now, 7*24*time.Hour))
	after, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal(int64(2), after.CursorSeq, "a quiet scope's cursor advances to the floor")
	assert.Equal(refreshed.Generation, after.Generation)
	delivery, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, after.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"unexpected"}`), nil
	})
	require.NoError(err)
	assert.Nil(delivery, "no gap reset for a quiet scope")
	input.ExpiresAt = now.Add(23 * time.Hour)
	_, truncated, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: after.Generation, ExpectedState: after.State, ExpectedSecretRevision: after.SecretRevision, Now: now})
	require.NoError(err)
	assert.False(truncated)
}

// A termination claimed after the subscription expired clears the marker and
// sends nothing.
func TestMCPEventsExpiredTerminationIsClaimedWithoutControl(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := draftProducerFixture(t, "slack")
	active, _ := mcpDraftSubscription(t, f)
	require.NoError(f.Store.PurgeChannelContext(t.Context(), f.Source.ID, "draft-conversation"))
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.Equal("terminate_pending", stopped.LastOutcome)

	delivery, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, stopped.Generation, stopped.ExpiresAt.Add(time.Second), func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return nil, nil
	})
	require.NoError(err)
	assert.Nil(delivery)
	after, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal("terminated", after.LastOutcome)
	assert.Equal("scope_removed", after.StopReason)
}
