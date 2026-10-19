package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor beside the local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor beside the local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsActivationRejectsStaleRuntimeAfterDurableDisable(t *testing.T) {
	for _, tc := range staleMCPEventConfigs() {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := storetest.New(t)
			grantSyntheticSourceFixture(t, f)
			_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)
			now := time.Now().UTC()
			input := newMCPStoreSubscription(t, f, 1, now)

			current, err := store.OpenForTest(store.DBPathForTest(f.Store))
			require.NoError(err)
			t.Cleanup(func() { Require.NoError(t, current.Close()) })
			_, err = current.ConfigureMCPEvents(t.Context(), tc.config)
			require.NoError(err)

			_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			require.Error(err)
			assert.Equal("subscription_unavailable", err.Error(), "a cached runtime cannot activate a subscription after durable capture changes")
			subscriptions, err := f.Store.ListMCPSubscriptions(t.Context(), input.Principal)
			require.NoError(err)
			assert.Empty(subscriptions)
		})
	}
}

func TestMCPEventsStaleLiveWriterDoesNotCaptureAfterDurableDisable(t *testing.T) {
	for _, tc := range staleMCPEventConfigs() {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			f := storetest.New(t)
			grantSyntheticSourceFixture(t, f)
			_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)

			current, err := store.OpenForTest(store.DBPathForTest(f.Store))
			require.NoError(err)
			t.Cleanup(func() { Require.NoError(t, current.Close()) })
			_, err = current.ConfigureMCPEvents(t.Context(), tc.config)
			require.NoError(err)

			messageID, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now().UTC()}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("synthetic-disabled-capture").Build()})
			require.NoError(err, "a stale Events snapshot must not block ordinary archive writes")
			assert.Positive(messageID)
			var eventCount int
			require.NoError(current.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log`).Scan(&eventCount))
			assert.Zero(eventCount, "the stale daemon must not append after durable capture configuration changes")
		})
	}
}

func staleMCPEventConfigs() []struct {
	name   string
	config store.MCPEventsConfig
} {
	return []struct {
		name   string
		config store.MCPEventsConfig
	}{
		{name: "disabled", config: store.MCPEventsConfig{}},
		{name: "live coverage changed", config: store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic"}},
	}
}
