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

func TestMCPEventsIdleCalendarResourceInvalidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	now := time.Now().UTC()
	cfg := mcpStoreConfig()
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: "msgvault.calendar_event_changed", SourceType: "gcal", Kinds: []string{"created", "updated", "cancelled"}})
	_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	source, err := f.Store.GetOrCreateSource("gcal", "owner@example.net/calendar")
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	input.Name = "msgvault.calendar_event_changed"
	input.ScopeKind = "source"
	input.SourceID = source.ID
	input.ScopeID = source.ID
	input.Arguments = []byte(fmt.Sprintf(`{"calendar_source_id":"%d"}`, source.ID))
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	require.NotNil(active)
	built := false
	build := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) { built = true; return nil, nil }
	delivery, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, build)
	require.NoError(err)
	assert.Nil(delivery)
	unchanged, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(unchanged)
	assert.Equal("active", unchanged.State)
	assert.Equal(active.Generation, unchanged.Generation)
	require.NoError(f.Store.UpdateSourceIdentifier(source.ID, "owner@example.net/replacement"))
	delivery, err = f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now.Add(time.Second), build)
	require.NoError(err)
	assert.Nil(delivery)
	assert.False(built)
	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State)
	assert.Equal("scope_removed", stopped.StopReason)
	assert.Greater(stopped.Generation, active.Generation)
}
