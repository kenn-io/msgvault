package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A calendar removed while its first subscription waits on the callback
// challenge has no subscription row to stop. If another calendar then takes
// the same source ID, activation must not bind to the replacement.
func TestMCPEventsCalendarActivationRejectsReplacedSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []store.MCPEventCapability{{Family: "msgvault.calendar_event_changed", SourceType: "gcal", Kinds: []string{"created", "updated", "cancelled"}}},
	})
	require.NoError(err)
	calendar, err := f.Store.GetOrCreateSource("gcal", "calendar-a@example.test/events")
	require.NoError(err)
	input := store.MCPSubscription{
		ID: "sub_calendar", Principal: "owner:synthetic", Name: "msgvault.calendar_event_changed", Arguments: []byte(`{}`),
		ScopeKind: "source", ScopeID: calendar.ID, CallbackURL: "https://receiver.example.net/events",
		SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))

	require.NoError(f.Store.RemoveSource(calendar.ID))
	if f.Store.IsPostgreSQL() {
		// PostgreSQL never reuses identity values; force the same ID.
		_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO sources (id, source_type, identifier) OVERRIDING SYSTEM VALUE VALUES (?, 'gcal', 'calendar-b@example.test/events')`), calendar.ID)
		require.NoError(err)
	} else {
		replacement, err := f.Store.GetOrCreateSource("gcal", "calendar-b@example.test/events")
		require.NoError(err)
		require.Equal(calendar.ID, replacement.ID, "SQLite reuses the removed highest source ID")
	}

	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.Error(err)
	assert.Equal("unknown_scope", err.Error())
}
