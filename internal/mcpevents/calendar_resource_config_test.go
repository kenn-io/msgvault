package mcpevents

import (
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
)

func TestCalendarResourceLifetimeTracksRoutingConfig(t *testing.T) {
	for _, tc := range []struct {
		name, next     string
		restore, valid bool
	}{
		{name: "account", next: `{"account_email":"replacement@example.net","calendar_id":"primary"}`},
		{name: "calendar", next: `{"account_email":"owner@example.net","calendar_id":"other"}`},
		{name: "account_away_back", next: `{"account_email":"replacement@example.net","calendar_id":"primary"}`, restore: true},
		{name: "calendar_away_back", next: `{"account_email":"owner@example.net","calendar_id":"other"}`, restore: true},
		{name: "display", next: `{"account_email":"owner@example.net","calendar_id":"primary","calendar_summary":"Renamed calendar","time_zone":"UTC"}`, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			service, fixture, request := eventService(t)
			source, err := fixture.Store.GetOrCreateSource("gcal", "owner@example.net/primary")
			require.NoError(err)
			const original = `{"account_email":"owner@example.net","calendar_id":"primary"}`
			require.NoError(fixture.Store.UpdateSourceSyncConfig(source.ID, original))
			request.Name = calendarFamily
			request.Arguments = map[string]any{"calendar_source_id": strconv.FormatInt(source.ID, 10)}
			first, err := service.Subscribe(t.Context(), service.principal, request)
			require.NoError(err)
			require.NoError(fixture.Store.UpdateSourceSyncConfig(source.ID, tc.next))
			if tc.restore {
				require.NoError(fixture.Store.UpdateSourceSyncConfig(source.ID, original))
			}
			result, err := service.Subscribe(t.Context(), service.principal, request)
			if tc.valid {
				require.NoError(err)
				assert.Equal(first.ID, result.ID)
			} else {
				require.Error(err, "calendar routing replacement cannot retain subscription authority")
			}
		})
	}
}
