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
			epoch := clock.Epoch
			if oldEpoch {
				epoch = initial.Epoch
			}
			active, truncated, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now, Replay: true, ReplayEpoch: epoch, ReplaySeq: 1})
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
