package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsFreshReplayTruncatesOldEpochOrPrunedCursor(t *testing.T) {
	for _, oldEpoch := range []bool{false, true} {
		name := "below floor"
		if oldEpoch {
			name = "old epoch"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			now := time.Now().UTC()
			cfg := mcpStoreConfig()
			initial, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			cfg.Enabled = false
			_, err = f.Store.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			cfg.Enabled = true
			clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			_, err = f.Store.DB().Exec(`UPDATE mcp_event_clock SET head_seq=3,pruned_through_seq=2 WHERE singleton=1`)
			require.NoError(err)
			// Each case breaks exactly one condition: an old epoch with a cursor
			// at the head, or the current epoch with a cursor below the floor.
			epoch, seq := clock.Epoch, int64(1)
			if oldEpoch {
				epoch, seq = initial.Epoch, 3
			}
			active, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now, Replay: true, ReplayEpoch: epoch, ReplaySeq: seq})
			require.NoError(err)
			assert.True(truncated)
			assert.Equal(clock.Epoch, active.CursorEpoch)
			assert.Equal(int64(3), active.CursorSeq)
		})
	}
}

func TestMCPEventsStoppedGapRefreshIgnoresSuppliedCursor(t *testing.T) {
	for _, reason := range []string{"retention", "capture_gap"} {
		t.Run(reason, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			now := time.Now().UTC()
			clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)
			input := newMCPStoreSubscription(t, f, 1, now)
			active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			require.NoError(err)
			_, err = f.Store.DB().Exec(`UPDATE mcp_event_clock SET head_seq=3 WHERE singleton=1`)
			require.NoError(err)
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason=?,generation=generation+1 WHERE id=?`), reason, active.ID)
			require.NoError(err)
			stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			restored, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, ExpectedGeneration: stopped.Generation, ExpectedState: stopped.State, ExpectedSecretRevision: stopped.SecretRevision, Now: now, Replay: true, ReplayEpoch: clock.Epoch, ReplaySeq: 1})
			require.NoError(err)
			assert.True(truncated)
			assert.Equal(int64(3), restored.CursorSeq)
		})
	}
}

func TestMCPEventsActivationRechecksScopeAfterSourceRemoval(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	// The candidate was verified outside a transaction while this real
	// source removal committed. Activation must validate its scope again.
	require.NoError(f.Store.RemoveSource(f.Source.ID))
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.Error(err)
	assert.Equal("unknown_scope", err.Error())
	rows, err := f.Store.ListMCPSubscriptions(t.Context(), input.Principal)
	require.NoError(err)
	assert.Empty(rows)
}

func TestActivateMaxAgeFloor(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name          string
		maxAge        time.Duration
		otherScope    bool
		olderFromMe   bool
		wantSeq       int64
		wantTruncated bool
	}{
		{"floor skips older rows", 3 * time.Minute, false, false, 2, true},
		{"floor older than every row", 30 * time.Minute, false, false, 0, false},
		{"older rows out of scope", 3 * time.Minute, true, false, 2, false},
		// include_from_me is false, so skipping the owner's own messages
		// loses nothing the subscription would receive.
		{"older rows excluded by filters", 3 * time.Minute, false, true, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)
			scope := f.ConvID
			if tc.otherScope {
				scope = f.ConvID + 1000
			}
			f.InsertMCPConversationEvent(clock.Epoch, 1, scope, now.Add(-10*time.Minute))
			f.InsertMCPConversationEvent(clock.Epoch, 2, scope, now.Add(-5*time.Minute))
			if tc.olderFromMe {
				_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_log SET from_me=TRUE WHERE seq<=2`))
				require.NoError(err)
			}
			if tc.otherScope {
				scope = f.ConvID
			}
			f.InsertMCPConversationEvent(clock.Epoch, 3, scope, now.Add(-time.Minute))
			active, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now, Replay: true, ReplayEpoch: clock.Epoch, ReplaySeq: 0, MaxAge: tc.maxAge})
			require.NoError(err)
			assert.Equal(tc.wantSeq, active.CursorSeq)
			assert.Equal(tc.wantTruncated, truncated)
		})
	}
}

func TestMaxAgeIgnoredWithoutCursor(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	f.InsertMCPConversationEvent(clock.Epoch, 1, f.ConvID, now.Add(-10*time.Minute))
	f.InsertMCPConversationEvent(clock.Epoch, 2, f.ConvID, now.Add(-5*time.Minute))
	active, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now, MaxAge: 3 * time.Minute})
	require.NoError(err)
	assert.Equal(int64(2), active.CursorSeq)
	assert.False(truncated)
}

// Reactivating an expired row resumes its stored cursor, and the maxAgeMs
// floor still raises that cursor. Truncation is reported only when an
// in-scope event is skipped.
func TestActivateMaxAgeFloorOnExpiredReactivation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		otherScope    bool
		wantTruncated bool
	}{
		{"in-scope events skipped", false, true},
		{"out-of-scope events skipped", true, false},
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
			require.Zero(active.CursorSeq)
			expiredAt := now.Add(25 * time.Hour)
			require.NoError(f.Store.ExpireMCPSubscriptions(t.Context(), expiredAt))
			expired, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			require.Equal("expired", expired.State)
			require.Zero(expired.CursorSeq)

			renewAt := expiredAt.Add(time.Hour)
			scope := f.ConvID
			if tc.otherScope {
				scope = f.ConvID + 1000
			}
			f.InsertMCPConversationEvent(clock.Epoch, 1, scope, renewAt.Add(-10*time.Minute))
			f.InsertMCPConversationEvent(clock.Epoch, 2, scope, renewAt.Add(-5*time.Minute))
			f.InsertMCPConversationEvent(clock.Epoch, 3, f.ConvID, renewAt.Add(-time.Minute))
			input.ExpiresAt = renewAt.Add(24 * time.Hour)
			renewed, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{
				Subscription: input, ExpectedGeneration: expired.Generation, ExpectedState: expired.State,
				ExpectedSecretRevision: expired.SecretRevision, Now: renewAt, MaxAge: 3 * time.Minute,
			})
			require.NoError(err)
			assert.Equal(int64(2), renewed.CursorSeq)
			assert.Equal(tc.wantTruncated, truncated)
		})
	}
}

// Retention deletes an inactive subscription after its grace period. The
// first activation sees that the predecessor it was challenged against is
// gone; a retry for a fresh subscription must then succeed instead of
// meeting the same stale row, because the maintenance was committed.
func TestMCPEventsActivationAfterGraceCommitsPruning(t *testing.T) {
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	cfg.Retention = 7 * 24 * time.Hour
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	generation, err := f.Store.EndMCPSubscription(t.Context(), input.ID, input.Principal, now)
	require.NoError(err)
	stale := now.Add(-48 * time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_subscriptions SET expires_at=?,updated_at=? WHERE id=?`), stale, stale, input.ID)
	require.NoError(err)
	f.InsertMCPConversationEvent(clock.Epoch, clock.HeadSeq+1, f.ConvID, now.Add(-8*24*time.Hour))

	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), ExpectedGeneration: generation, ExpectedState: "unsubscribed", ExpectedSecretRevision: 1, Now: now})
	require.ErrorContains(err, "concurrent_update")
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now})
	require.NoError(err, "the retry must not meet the predecessor retention already removed")
	require.Equal(int64(1), active.Generation)
}

// A principal change revokes the old owner's subscriptions for good. If that
// owner key is configured again, unsubscribing a revoked row must not clear
// the revocation and let a later activation restore it.
func TestMCPEventsUnsubscribeKeepsPrincipalRevocation(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	owner := mcpStoreConfig()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), owner)
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	rotated := owner
	rotated.Principal = "owner:rotated"
	_, err = f.Store.ConfigureMCPEvents(t.Context(), rotated)
	require.NoError(err)
	_, err = f.Store.ConfigureMCPEvents(t.Context(), owner)
	require.NoError(err)

	_, err = f.Store.EndMCPSubscription(t.Context(), input.ID, input.Principal, now)
	require.ErrorContains(err, "subscription_not_found")
	row, err := f.Store.GetMCPSubscription(t.Context(), input.ID)
	require.NoError(err)
	require.NotNil(row)
	assert.Equal("principal_revoked", row.StopReason)
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), ExpectedGeneration: row.Generation, ExpectedState: row.State, ExpectedSecretRevision: row.SecretRevision, Now: now})
	require.ErrorContains(err, "subscription_unavailable")
}
