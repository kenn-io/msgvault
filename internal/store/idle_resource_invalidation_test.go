package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsInvalidatedResourceStopsWithoutReadyDelivery(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "empty"
		if pending {
			name = "future_retry"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			grantSyntheticSourceFixture(t, f)
			now := time.Now().UTC()
			clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)
			input := newMCPStoreSubscription(t, f, 1, now)
			active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			require.NoError(err)
			require.NotNil(active)
			if pending {
				appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
				delivery, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
					return []byte(`{"eventId":"synthetic"}`), nil
				})
				require.NoError(err)
				require.NotNil(delivery)
				require.NoError(f.Store.FinishMCPDelivery(t.Context(), active.ID, active.Generation, delivery.Event.Seq, now, 503, now.Add(time.Hour)))
			}
			require.NoError(f.Store.UpdateSourceIdentifier(f.Source.ID, "replacement@example.net"))
			built := false
			delivery, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now.Add(time.Second), func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
				built = true
				return nil, nil
			})
			require.NoError(err)
			assert.Nil(delivery)
			assert.False(built)
			stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			require.NotNil(stopped)
			assert.Equal("stopped", stopped.State)
			assert.Equal("scope_removed", stopped.StopReason)
			assert.Greater(stopped.Generation, active.Generation)
			assert.Empty(stopped.PendingEnvelope)
		})
	}
}
