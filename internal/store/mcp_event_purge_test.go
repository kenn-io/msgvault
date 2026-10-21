package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsPurgePreservesExpiryAndEndGrace(t *testing.T) {
	for _, lateExpiry := range []bool{false, true} {
		name := "unsubscribed"
		if lateExpiry {
			name = "late expiry processing"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			now := time.Now().UTC()
			_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
			require.NoError(err)
			input := newMCPStoreSubscription(t, f, 1, now)
			active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			require.NoError(err)
			endedAt := now
			if lateExpiry {
				endedAt = now.Add(72 * time.Hour)
				require.NoError(f.Store.ExpireMCPSubscriptions(t.Context(), endedAt))
			} else {
				require.NoError(f.Store.EndMCPSubscription(t.Context(), active.ID, active.Principal, now))
			}
			purgeAt := input.ExpiresAt.Add(24 * time.Hour)
			if minimum := endedAt.Add(24 * time.Hour); minimum.After(purgeAt) {
				purgeAt = minimum
			}
			require.NoError(f.Store.PruneMCPEvents(t.Context(), purgeAt.Add(-time.Nanosecond), 7*24*time.Hour))
			retained, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			assert.NotNil(retained)
			require.NoError(f.Store.PruneMCPEvents(t.Context(), purgeAt, 7*24*time.Hour))
			purged, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
			require.NoError(err)
			assert.Nil(purged)
		})
	}
}
