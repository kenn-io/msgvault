package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsSerializedSourceRemovalEndsPendingAfterMessageDeletion(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	messageID := f.CreateMessage("synthetic-serialized-source-target")
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_live_admissions (message_id,source_id,message_reference_seq,epoch,admitted_at) VALUES (?,?,1,?,?)`), messageID, f.Source.ID, clock.Epoch, now.Format(time.RFC3339Nano))
	require.NoError(err)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic-pending"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id=?`), messageID)
	require.NoError(err)
	_, _, err = f.Store.RemoveSourceSerialized(t.Context(), f.Source.ID)
	require.NoError(err)
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State)
	assert.Equal("scope_removed", stopped.StopReason)
	assert.Greater(stopped.Generation, active.Generation)
	assert.Empty(stopped.PendingEnvelope)
	for _, table := range []string{"mcp_event_log", "mcp_live_admissions"} {
		var count int
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count))
		assert.Zero(count)
	}
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, active.Principal, now)
	require.Error(err)
	require.NoError(f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, 1, now, 204, time.Time{}))
	unchanged, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal(stopped.Generation, unchanged.Generation)
}
