package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func pendingMCPAuthorityFixture(t *testing.T, ttl time.Duration) (*storetest.Fixture, *store.MCPSubscription) {
	t.Helper()
	require := Require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	input := newMCPStoreSubscription(t, f, 1, now)
	input.ExpiresAt = now.Add(ttl)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	require.NotNil(active)
	appendMCPSubscriptionFixture(t, f, clock.Epoch, now)
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic-pending"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)
	return f, &pending.Subscription
}

// A retained receipt does not extend subscription authority while the expiry
// sweeper has not run. Checking the occurrence must leave its pending bytes intact.
func TestMCPDeliveryOccurrenceRejectsExpiredSubscription(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"before_expiry", -time.Nanosecond},
		{"at_expiry", 0},
		{"after_expiry", time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f, pending := pendingMCPAuthorityFixture(t, time.Hour)
			err := f.Store.CheckMCPDeliveryOccurrence(t.Context(), pending.ID, pending.Generation, pending.PendingSeq, pending.ExpiresAt.Add(tc.offset))
			if tc.offset < 0 {
				require.NoError(err)
			} else {
				require.EqualError(err, "subscription_inactive")
			}
			after, err := f.Store.GetMCPSubscription(t.Context(), pending.ID)
			require.NoError(err)
			assert.Equal(pending, after, "a read-only authority check must preserve the complete pending receipt")
		})
	}
}

// Generate valid subscription lifetimes and check times across expiry. The
// oracle depends only on the supplied lifetime, not the production validator.
func FuzzMCPDeliveryOccurrenceExpiration(f *testing.F) {
	f.Add(uint64(time.Hour), int64(-1))
	f.Add(uint64(time.Hour), int64(0))
	f.Add(uint64(24*time.Hour), int64(1))
	f.Add(uint64(1), int64(0))
	f.Fuzz(func(t *testing.T, lifetime uint64, offset int64) {
		assert := Assert.New(t)
		// Materialize a valid lifetime in the native Store's (0, 24h] domain.
		ttl := time.Duration(lifetime%uint64(24*time.Hour)) + time.Nanosecond
		fixture, pending := pendingMCPAuthorityFixture(t, ttl)
		err := fixture.Store.CheckMCPDeliveryOccurrence(t.Context(), pending.ID, pending.Generation, pending.PendingSeq, pending.ExpiresAt.Add(time.Duration(offset)))
		if offset < 0 {
			assert.NoError(err)
		} else {
			assert.EqualError(err, "subscription_inactive")
		}
	})
}
