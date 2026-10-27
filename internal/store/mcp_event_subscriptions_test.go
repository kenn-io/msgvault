package store_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func mcpStoreConfig() store.MCPEventsConfig {
	return store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}}
}

func TestMCPEventsCoverageChangesEpochOnce(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	cfg := mcpStoreConfig()
	first, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	same, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(first.Epoch, same.Epoch)
	cfg.Enabled = false
	disabled, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(first.Epoch+1, disabled.Epoch)
	sameDisabled, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(disabled.Epoch, sameDisabled.Epoch)
	cfg.Enabled = true
	resumed, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(disabled.Epoch+1, resumed.Epoch)
}

func newMCPStoreSubscription(t *testing.T, f *storetest.Fixture, id int, now time.Time) store.MCPSubscription {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{"conversation_id": strconv.FormatInt(f.ConvID, 10), "include_from_me": false})
	Require.NoError(t, err)
	sub := store.MCPSubscription{ID: fmt.Sprintf("sub_%064x", id), Principal: "owner:synthetic", Name: "msgvault.message_archived", Arguments: arguments, ScopeKind: "conversation", ScopeID: f.ConvID, SourceID: f.Source.ID, CallbackURL: fmt.Sprintf("https://receiver.example.net/hook/%d", id), SecretEnc: []byte("synthetic-encrypted-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(24 * time.Hour)}
	Require.NoError(t, f.Store.BindMCPSubscriptionScope(t.Context(), &sub))
	return sub
}

func TestMCPEventsRenewalPreservesPendingGeneration(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	assert.False(truncated)
	require.NotNil(active)
	assert.Equal(clock.HeadSeq, active.CursorSeq)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_clock SET head_seq = 1 WHERE singleton = 1`))
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (1,?,'msgvault.message_archived','message','conversation',?,'message:1',1,?,?,FALSE,?,?,?)`), clock.Epoch, f.ConvID, f.ConvID, f.Source.ID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), `{"kind":"message","from_me":false}`)
	require.NoError(err)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"stable"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)
	input.ExpiresAt = now.Add(25 * time.Hour)
	renewed, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: active.Generation, ExpectedState: "active", ExpectedSecretRevision: 1, Now: now.Add(time.Hour)})
	require.NoError(err)
	assert.Equal(active.Generation, renewed.Generation)
	assert.Equal(pending.Subscription.PendingEnvelope, renewed.PendingEnvelope)
	assert.Equal(int64(1), renewed.PendingSeq)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now.Add(time.Hour), 200, time.Time{})
	require.NoError(err)
	settled, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(settled)
	assert.Equal(int64(1), settled.CursorSeq)
	assert.Empty(settled.PendingEnvelope)
}

func TestMCPEventsRenewalAppliesRetentionPruneAfterValidatingPredecessor(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	cfg.Retention = time.Hour
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"expires-with-retention"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)

	input.ExpiresAt = now.Add(24 * time.Hour)
	renewed, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{
		Subscription: input, ExpectedGeneration: active.Generation, ExpectedState: active.State,
		ExpectedSecretRevision: active.SecretRevision, Now: now.Add(2 * time.Hour),
	})
	require.NoError(err)
	require.NotNil(renewed)
	assert.True(truncated, "renewal that crosses the retention floor resumes at the current head")
	assert.Equal("active", renewed.State)
	assert.Equal(clock.Epoch, renewed.CursorEpoch)
	assert.Equal(int64(1), renewed.CursorSeq)
	assert.Empty(renewed.PendingEnvelope, "pruned occurrences cannot retain a delivery receipt")

	var floor int64
	require.NoError(f.Store.DB().QueryRow(`SELECT pruned_through_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&floor))
	assert.Equal(int64(1), floor)
	var occurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&occurrences))
	assert.Zero(occurrences)
}

func TestMCPEventsConcurrentActivationsCannotExceedQuota(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	for id := 1; id <= 63; id++ {
		_, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, id, now), Now: now})
		require.NoError(err)
	}
	inputs := []store.MCPSubscription{newMCPStoreSubscription(t, f, 64, now), newMCPStoreSubscription(t, f, 65, now)}
	start := make(chan struct{})
	results := make(chan error, len(inputs))
	for _, input := range inputs {
		go func() {
			<-start
			_, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			results <- err
		}()
	}
	close(start)
	accepted := 0
	for range inputs {
		err := <-results
		if err == nil {
			accepted++
		} else {
			assert.Equal("subscription_limit", err.Error())
		}
	}
	assert.Equal(1, accepted)
	rows, err := f.Store.ListMCPSubscriptions(t.Context(), "owner:synthetic")
	require.NoError(err)
	assert.Len(rows, 64)
}

func TestMCPEventsStaleCandidateCannotReactivateUnsubscribedGeneration(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	_, err = f.Store.EndMCPSubscription(t.Context(), active.ID, input.Principal, now)
	require.NoError(err)
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: active.Generation, ExpectedState: "active", ExpectedSecretRevision: 1, Now: now})
	require.Error(err)
	assert.Equal("concurrent_update", err.Error())
	ended, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(ended)
	assert.Equal("unsubscribed", ended.State)
	assert.Greater(ended.Generation, active.Generation)
}

func TestMCPEventsPruningHoleMarksLaggingSubscriberForGap(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	// Leave activation reads above the synthetic event age so the explicit
	// seven-day sweep below is the operation tested here.
	cfg.Retention = 9 * 24 * time.Hour
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	for seq := int64(1); seq <= 3; seq++ {
		recorded := now
		if seq == 2 {
			recorded = now.Add(-8 * 24 * time.Hour)
		}
		_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (?,?,'msgvault.message_archived','message','conversation',?,?,?, ?,?,FALSE,?,?,?)`), seq, clock.Epoch, f.ConvID, fmt.Sprintf("message:%d", seq), seq, f.ConvID, f.Source.ID, now.Format(time.RFC3339Nano), recorded.Format(time.RFC3339Nano), `{"kind":"message","from_me":false}`)
		require.NoError(err)
	}
	_, err = f.Store.DB().Exec(`UPDATE mcp_event_clock SET head_seq=3 WHERE singleton=1`)
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now, Replay: true, ReplayEpoch: clock.Epoch, ReplaySeq: 1})
	require.NoError(err)
	require.NoError(f.Store.PruneMCPEvents(t.Context(), now, 7*24*time.Hour))
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("active", stopped.State, "a sweep keeps a lossy subscription active so the worker can report the gap")
	assert.Empty(stopped.StopReason)
	assert.Equal("gap_pending", stopped.LastOutcome)
	assert.Equal(int64(1), stopped.CursorSeq)
	restored, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now, ExpectedGeneration: stopped.Generation, ExpectedState: stopped.State, ExpectedSecretRevision: stopped.SecretRevision})
	require.NoError(err)
	assert.True(truncated)
	assert.Equal(int64(3), restored.CursorSeq)
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(2, count)
}

func appendMCPSubscriptionFixture(t *testing.T, f *storetest.Fixture, epoch int64, now time.Time) {
	t.Helper()
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (1,?,'msgvault.message_archived','message','conversation',?,'message:1',1,?,?,FALSE,?,?,?)`), epoch, f.ConvID, f.ConvID, f.Source.ID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), `{"kind":"message","from_me":false}`)
	Require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE mcp_event_clock SET head_seq=1 WHERE singleton=1`)
	Require.NoError(t, err)
}

func TestMCPEventsRotationRebindsPendingAndRejectsOldCompletion(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	bytes := []byte(`{"eventId":"stable","cursor":"stable"}`)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) { return bytes, nil })
	require.NoError(err)
	require.NotNil(pending)
	due := now.Add(time.Minute)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 503, due)
	require.NoError(err)
	input.SecretEnc = []byte("synthetic-new-encrypted-secret")
	input.SecretRevision = 2
	input.VerifiedRevision = 2
	input.PreviousSecretEnc = active.SecretEnc
	input.PreviousSecretUntil = now.Add(time.Minute)
	rotated, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: active.Generation, ExpectedState: active.State, ExpectedSecretRevision: active.SecretRevision, Now: now})
	require.NoError(err)
	assert.Equal(active.Generation+1, rotated.Generation)
	assert.Equal(rotated.Generation, rotated.PendingGeneration)
	assert.Equal(bytes, rotated.PendingEnvelope)
	assert.Equal(1, rotated.AttemptCount)
	assert.WithinDuration(due, rotated.NextAttemptAt, time.Millisecond)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 200, time.Time{})
	require.NoError(err)
	unchanged, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal(int64(0), unchanged.CursorSeq)
	assert.Equal(bytes, unchanged.PendingEnvelope)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, rotated.Generation, 1, due, 204, time.Time{})
	require.NoError(err)
	settled, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal(int64(1), settled.CursorSeq)
}

func TestMCPEventsCoverageChangeInvalidatesPendingCompletion(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"stable"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)
	cfg.Enabled = false
	_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	control, err := f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 200, time.Time{})
	require.NoError(err)
	assert.NotNil(control, "an epoch change during the request reports a gap")
	reset, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal("active", reset.State)
	assert.Equal("gap", reset.LastOutcome)
	assert.Greater(reset.Generation, active.Generation)
	assert.Empty(reset.PendingEnvelope)
	cfg.Enabled = true
	_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	resumed, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: reset.Generation, ExpectedState: reset.State, ExpectedSecretRevision: reset.SecretRevision, Now: now})
	require.NoError(err)
	assert.True(truncated)
	assert.Equal(int64(1), resumed.CursorSeq)
}

func TestMCPEventsOwnerRotationRevokesOldPendingWithoutCoverageChange(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"old-owner-event"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)
	cfg.Principal = "owner:new-synthetic-key"
	restarted, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(clock.Epoch, restarted.Epoch)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 200, time.Time{})
	require.NoError(err)
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal("stopped", stopped.State)
	assert.Equal("principal_revoked", stopped.StopReason)
	assert.Equal(int64(0), stopped.CursorSeq)
	assert.Greater(stopped.Generation, active.Generation)
	assert.Empty(stopped.PendingEnvelope)
	for _, principal := range []string{input.Principal, cfg.Principal} {
		_, err := f.Store.GetMCPEvent(t.Context(), active.ID, 1, principal, now)
		require.Error(err)
	}
}

func TestMCPEventsScopeRequiresAdvertisedSourceAndKind(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	sourceType, sourceID, err := f.Store.ValidateMCPEventScope(t.Context(), "msgvault.message_archived", "conversation", f.ConvID, []string{"message"})
	require.NoError(err)
	assert.Equal("gmail", sourceType)
	assert.Equal(f.Source.ID, sourceID)
	for _, tc := range []struct {
		name, family, scope, reason string
		id                          int64
		kinds                       []string
	}{
		{"missing conversation", "msgvault.message_archived", "conversation", "unknown_scope", 9223372036854775807, []string{"message"}},
		{"wrong scope", "msgvault.message_archived", "source", "wrong_scope_type", f.Source.ID, []string{"message"}},
		{"unadvertised reaction", "msgvault.message_archived", "conversation", "kind_not_capable", f.ConvID, []string{"reaction"}},
		{"unadvertised draft", "msgvault.draft_changed", "conversation", "source_not_capable", f.ConvID, []string{"created"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.Store.ValidateMCPEventScope(t.Context(), tc.family, tc.scope, tc.id, tc.kinds)
			Require.Error(t, err)
			Assert.Equal(t, tc.reason, err.Error())
		})
	}
}

func TestMCPEventsRetainedReceiptSurvivesFloorButNotUnsubscribe(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	// Retention may leave a recent physical receipt below a later deleted seq.
	_, err = f.Store.DB().Exec(`UPDATE mcp_event_clock SET head_seq=2, pruned_through_seq=2 WHERE singleton=1`)
	require.NoError(err)
	receipt, err := f.Store.GetMCPEvent(t.Context(), active.ID, 1, input.Principal, now)
	require.NoError(err)
	assert.Equal(int64(1), receipt.Seq)
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, "owner:another-synthetic", now)
	require.Error(err)
	assert.NotContains(err.Error(), input.Principal)
	_, err = f.Store.EndMCPSubscription(t.Context(), active.ID, input.Principal, now)
	require.NoError(err)
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, input.Principal, now)
	require.Error(err)
}

func TestMCPEventsExpiredReceiptHasFiniteReadGrace(t *testing.T) {
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	input.ExpiresAt = now.Add(time.Minute)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	_, err = f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, input.ExpiresAt, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		Require.FailNow(t, "an expired subscription must not build a delivery")
		return nil, nil
	})
	require.NoError(err)
	expired, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	Assert.Equal(t, "expired", expired.State)
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, input.Principal, input.ExpiresAt.Add(23*time.Hour))
	require.NoError(err)
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, input.Principal, input.ExpiresAt.Add(24*time.Hour))
	require.Error(err)
}

func TestMCPEventsFailedActivationDoesNotLogCallbackOrDriverText(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	marker := "synthetic-private-driver-marker"
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`CREATE FUNCTION reject_mcp_subscription() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic-private-driver-marker'; END $$`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_mcp_subscription BEFORE INSERT ON mcp_event_subscriptions FOR EACH ROW EXECUTE FUNCTION reject_mcp_subscription()`)
	} else {
		_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_mcp_subscription BEFORE INSERT ON mcp_event_subscriptions BEGIN SELECT RAISE(ABORT, 'synthetic-private-driver-marker'); END`)
	}
	require.NoError(err)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	input := newMCPStoreSubscription(t, f, 1, now)
	input.CallbackURL = "https://receiver.example.net/private-callback-marker"
	input.SecretEnc = []byte("synthetic-secret-ciphertext-marker")
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.Error(err)
	assert.NotContains(err.Error(), marker)
	for _, sensitive := range []string{marker, input.CallbackURL, string(input.SecretEnc)} {
		assert.NotContains(logs.String(), sensitive)
	}
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_subscriptions`).Scan(&count))
	assert.Zero(count)
}

func TestMCPEventsSourceRemovalCleansReceiptsAfterMessageDeletion(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	messageID := f.CreateMessage("synthetic-receipt-target")
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id=?`), messageID)
	require.NoError(err)
	require.NoError(f.Store.RemoveSource(f.Source.ID))
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State)
	assert.Equal("scope_removed", stopped.StopReason)
	assert.Greater(stopped.Generation, active.Generation)
	for _, table := range []string{"mcp_event_log"} {
		var count int
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count))
		assert.Zero(count)
	}
}

func TestMCPEventsCoverageFingerprintIgnoresMatrixOrder(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	cfg := mcpStoreConfig()
	cfg.Capabilities = []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message", "reaction"}},
		{Family: "msgvault.calendar_event_changed", SourceType: "gcal", Kinds: []string{"created", "updated", "cancelled"}},
	}
	first, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	cfg.Capabilities = []store.MCPEventCapability{
		{Family: "msgvault.calendar_event_changed", SourceType: "gcal", Kinds: []string{"cancelled", "updated", "created"}},
		{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"reaction", "message"}},
	}
	reordered, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(first.Epoch, reordered.Epoch)
	cfg.Capabilities[1].Kinds = []string{"message"}
	narrowed, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(first.Epoch+1, narrowed.Epoch)
}

func TestMCPEventsDisabledArchiveWriterDoesNotLockEventClock(t *testing.T) {
	require := Require.New(t)
	f := storetest.New(t)
	cfg := mcpStoreConfig()
	cfg.Enabled = false
	_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`CREATE FUNCTION reject_mcp_clock_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'disabled event clock must not be locked'; END $$`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_mcp_clock_write BEFORE UPDATE ON mcp_event_clock FOR EACH ROW EXECUTE FUNCTION reject_mcp_clock_write()`)
	} else {
		_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_mcp_clock_write BEFORE UPDATE ON mcp_event_clock BEGIN SELECT RAISE(ABORT, 'disabled event clock must not be locked'); END`)
	}
	require.NoError(err)
	Assert.Positive(t, f.CreateMessage("synthetic-disabled-capture"))
}

func TestMCPEventsOwnMessageGuardDoesNotCountRetries(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	input.Arguments = []byte(fmt.Sprintf(`{"conversation_id":%q,"include_from_me":true}`, strconv.FormatInt(f.ConvID, 10)))
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	_, err = f.Store.DB().Exec(`UPDATE mcp_event_log SET from_me=TRUE, data='{"kind":"message","from_me":true}' WHERE seq=1`)
	require.NoError(err)
	builds := 0
	build := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		builds++
		return []byte(`{"eventId":"stable-own-event"}`), nil
	}
	first, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, build)
	require.NoError(err)
	require.NotNil(first)
	assert.Equal(1, first.Subscription.FromMeWindowCount)
	due := now.Add(time.Minute)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 503, due)
	require.NoError(err)
	retry, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, due, build)
	require.NoError(err)
	require.NotNil(retry)
	assert.Equal(1, retry.Subscription.FromMeWindowCount)
	assert.Equal(first.Subscription.PendingEnvelope, retry.Subscription.PendingEnvelope)
	assert.Equal(1, builds)
	assert.Equal(2, retry.Subscription.AttemptCount)
}

func TestMCPEventsTerminalDeliveryPoliciesPreserveLossEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		status, attempts, dead int
		state                  string
		cursor                 int64
	}{
		{name: "payload rejected", status: 413, attempts: 1, dead: 1, state: "active", cursor: 1},
		{name: "retry budget exhausted", status: 503, attempts: store.MCPMaxDeliveryAttempts, dead: 1, state: "active", cursor: 1},
		{name: "receiver gone", status: 410, attempts: 1, dead: 1, state: "active", cursor: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			now := time.Now().UTC()
			clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)
			input := newMCPStoreSubscription(t, f, 1, now)
			active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			require.NoError(err)
			appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
			build := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"bounded-attempt-event"}`), nil
			}
			for attempt := 1; attempt <= tc.attempts; attempt++ {
				pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, build)
				require.NoError(err)
				require.NotNil(pending)
				assert.Equal(attempt, pending.Subscription.AttemptCount)
				due := now.Add(time.Second)
				_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, tc.status, due)
				require.NoError(err)
				now = due
			}
			settled, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			assert.Equal(tc.state, settled.State)
			assert.Equal(tc.cursor, settled.CursorSeq)
			assert.Equal(tc.dead, settled.DeadLetterCount)
			assert.Empty(settled.PendingEnvelope)
			var count int
			require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_dead_letters`).Scan(&count))
			assert.Equal(tc.dead, count)
		})
	}
}

func TestMCPEventsIdleDeliveryDoesNotWrite(t *testing.T) {
	for _, mode := range []string{"empty", "unrelated", "retry"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			now := time.Now().UTC()
			clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)
			input := newMCPStoreSubscription(t, f, 1, now)
			active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			require.NoError(err)
			build := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				return []byte(`{"eventId":"stable"}`), nil
			}
			if mode != "empty" {
				appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
			}
			if mode == "unrelated" {
				_, err = f.Store.DB().Exec(`UPDATE mcp_event_log SET family='msgvault.draft_changed'`)
				require.NoError(err)
			}
			if mode == "retry" {
				pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, build)
				require.NoError(err)
				require.NotNil(pending)
				_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 503, now.Add(time.Hour))
				require.NoError(err)
			}
			_, err = f.Store.DB().Exec(`CREATE TABLE synthetic_poll_writes (n INTEGER NOT NULL)`)
			require.NoError(err)
			_, err = f.Store.DB().Exec(`INSERT INTO synthetic_poll_writes VALUES (0)`)
			require.NoError(err)
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION count_poll_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE synthetic_poll_writes SET n=n+1; RETURN NEW; END $$`)
				require.NoError(err)
			}
			for _, table := range []string{"archive_metadata", "mcp_event_clock", "mcp_event_subscriptions"} {
				statement := fmt.Sprintf(`CREATE TRIGGER count_poll_write AFTER UPDATE ON %s BEGIN UPDATE synthetic_poll_writes SET n=n+1; END`, table)
				if f.Store.IsPostgreSQL() {
					statement = fmt.Sprintf(`CREATE TRIGGER count_poll_write AFTER UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION count_poll_write()`, table)
				}
				// SQLite trigger names are database-wide.
				statement = strings.Replace(statement, "CREATE TRIGGER count_poll_write ", "CREATE TRIGGER count_poll_write_"+table+" ", 1)
				_, err = f.Store.DB().Exec(statement)
				require.NoError(err)
			}
			pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, build)
			require.NoError(err)
			assert.Nil(pending)
			var writes int
			require.NoError(f.Store.DB().QueryRow(`SELECT n FROM synthetic_poll_writes`).Scan(&writes))
			assert.Zero(writes, "idle and backoff polls must not acquire write fences or rewrite subscriptions")
			if mode == "empty" {
				appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
			}
			if mode == "unrelated" {
				_, err = f.Store.DB().Exec(`UPDATE mcp_event_log SET family='msgvault.message_archived'`)
				require.NoError(err)
			}
			_, err = f.Store.DB().Exec(`UPDATE synthetic_poll_writes SET n=0`)
			require.NoError(err)
			pending, err = f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now.Add(time.Hour), build)
			require.NoError(err)
			require.NotNil(pending, "ready deliveries must still use the fenced transaction")
			require.NoError(f.Store.DB().QueryRow(`SELECT n FROM synthetic_poll_writes`).Scan(&writes))
			assert.Positive(writes)
		})
	}
}

// A receiver that answers 410 ends the subscription, and ending a
// subscription removes its receipt read authority at once.
func TestMCPEventsGoneSubscriptionCannotReadReceipts(t *testing.T) {
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now})
	require.NoError(err)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, active.Principal, now)
	require.NoError(err, "an active subscription reads its receipt")
	// A 410 no longer ends a subscription, but rows from earlier versions can
	// still carry the retained state, and they must not read receipts.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET state='gone' WHERE id=?`), active.ID)
	require.NoError(err)
	gone, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(gone)
	require.Equal("gone", gone.State)

	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, active.Principal, now)
	require.Error(err)
	Assert.Equal(t, "event_unavailable", err.Error())
}

func insertMCPSubscriptionEvents(t *testing.T, f *storetest.Fixture, epoch int64, now time.Time, upTo int64) {
	t.Helper()
	require := Require.New(t)
	for seq := int64(1); seq <= upTo; seq++ {
		_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (?,?,'msgvault.message_archived','message','conversation',?,?,?,?,?,FALSE,?,?,?)`), seq, epoch, f.ConvID, fmt.Sprintf("message:%d", seq), seq, f.ConvID, f.Source.ID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), `{"kind":"message","from_me":false}`)
		require.NoError(err)
	}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_clock SET head_seq=? WHERE singleton=1`), upTo)
	require.NoError(err)
}

func TestLiveRefreshWithCurrentCursorIsNoOp(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	insertMCPSubscriptionEvents(t, f, clock.Epoch, now, 1)
	prepared, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"stable"}`), nil
	})
	require.NoError(err)
	require.NotNil(prepared)
	due := now.Add(time.Minute)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 500, due)
	require.NoError(err)
	prepared, err = f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, due, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"stable"}`), nil
	})
	require.NoError(err)
	require.NotNil(prepared)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, due, 500, due.Add(time.Minute))
	require.NoError(err)
	before, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.Equal(2, before.AttemptCount)

	later := now.Add(time.Second)
	input.ExpiresAt = later.Add(24 * time.Hour)
	for _, seq := range []int64{before.CursorSeq, before.PendingSeq} {
		refreshed, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: before.Generation, ExpectedState: before.State, ExpectedSecretRevision: before.SecretRevision, Now: later, Replay: true, ReplayEpoch: clock.Epoch, ReplaySeq: seq})
		require.NoError(err)
		assert.False(truncated)
		assert.Equal(before.Generation, refreshed.Generation)
		assert.Equal(before.CursorSeq, refreshed.CursorSeq)
		assert.Equal(before.PendingSeq, refreshed.PendingSeq)
		assert.Equal(before.PendingGeneration, refreshed.PendingGeneration)
		assert.Equal(2, refreshed.AttemptCount)
		assert.True(before.NextAttemptAt.Equal(refreshed.NextAttemptAt))
		assert.True(refreshed.ExpiresAt.After(before.ExpiresAt), "the TTL is extended")
	}
}

func TestLiveRefreshWithOlderCursorInCoverageIsNoOp(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	insertMCPSubscriptionEvents(t, f, clock.Epoch, now, 3)
	for seq := int64(1); seq <= 2; seq++ {
		prepared, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
			return []byte(`{"eventId":"stable"}`), nil
		})
		require.NoError(err)
		require.NotNil(prepared)
		_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, seq, now, 204, time.Time{})
		require.NoError(err)
	}
	before, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.Equal(int64(2), before.CursorSeq)

	// The client's persisted cursor lags the server; delivery still continues
	// from the settled position, so events 1 and 2 are not delivered again.
	maxAge := 24 * time.Hour
	refreshed, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: before.Generation, ExpectedState: before.State, ExpectedSecretRevision: before.SecretRevision, Now: now.Add(time.Second), Replay: true, ReplayEpoch: clock.Epoch, ReplaySeq: 1, MaxAge: maxAge})
	require.NoError(err)
	assert.False(truncated)
	assert.Equal(before.Generation, refreshed.Generation)
	assert.Equal(int64(2), refreshed.CursorSeq)
}

func TestLiveRefreshWithRotationKeepsCursorAndPending(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	insertMCPSubscriptionEvents(t, f, clock.Epoch, now, 1)
	prepared, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"stable"}`), nil
	})
	require.NoError(err)
	require.NotNil(prepared)
	input.SecretEnc = []byte("synthetic-new-encrypted-secret")
	input.SecretRevision = 2
	input.VerifiedRevision = 2
	rotated, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: active.Generation, ExpectedState: active.State, ExpectedSecretRevision: active.SecretRevision, Now: now, Replay: true, ReplayEpoch: clock.Epoch, ReplaySeq: 0})
	require.NoError(err)
	assert.False(truncated)
	assert.Equal(active.Generation+1, rotated.Generation)
	assert.Equal(rotated.Generation, rotated.PendingGeneration)
	assert.Equal(int64(1), rotated.PendingSeq)
	assert.Equal(int64(0), rotated.CursorSeq)
}

func TestExpiredRowReactivationWithCursorRewinds(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	insertMCPSubscriptionEvents(t, f, clock.Epoch, now, 2)
	prepared, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"stable"}`), nil
	})
	require.NoError(err)
	require.NotNil(prepared)
	_, err = f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 204, time.Time{})
	require.NoError(err)
	settled, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.Equal(int64(1), settled.CursorSeq)

	// Past its TTL the row is no longer live, so the client's cursor wins.
	after := settled.ExpiresAt.Add(time.Minute)
	input.ExpiresAt = after.Add(24 * time.Hour)
	reactivated, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: settled.Generation, ExpectedState: settled.State, ExpectedSecretRevision: settled.SecretRevision, Now: after, Replay: true, ReplayEpoch: clock.Epoch, ReplaySeq: 0})
	require.NoError(err)
	assert.False(truncated)
	assert.Equal(settled.Generation+1, reactivated.Generation)
	assert.Equal(int64(0), reactivated.CursorSeq)
}

func TestMCPEventsEndSubscriptionWithoutLiveRow(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *storetest.Fixture) (id, principal string)
	}{
		{"missing row", func(t *testing.T, f *storetest.Fixture) (string, string) {
			t.Helper()
			return "sub_missing", newMCPStoreSubscription(t, f, 1, now).Principal
		}},
		{"legacy gone row", func(t *testing.T, f *storetest.Fixture) (string, string) {
			t.Helper()
			input := newMCPStoreSubscription(t, f, 1, now)
			active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			Require.NoError(t, err)
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET state='gone' WHERE id=?`), active.ID)
			Require.NoError(t, err)
			return active.ID, input.Principal
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := storetest.New(t)
			_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			Require.NoError(t, err)
			id, principal := tc.setup(t, f)
			_, err = f.Store.EndMCPSubscription(t.Context(), id, principal, now)
			Require.Error(t, err)
			Assert.Equal(t, "subscription_not_found", err.Error())
		})
	}
}
