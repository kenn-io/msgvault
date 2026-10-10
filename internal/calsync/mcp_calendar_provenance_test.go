package calsync

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPCalendarProviderFullMutedIncrementalAndWriteThroughLive(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	m := gcal.NewMockAPI()
	calendar := gcal.Calendar{ID: "primary", AccessRole: "owner"}
	m.Calendars = []gcal.Calendar{calendar}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("historical", "Historical synthetic meeting")}}
	m.FullSyncToken["primary"] = "T1"
	s, st := newSyncer(t, m, Options{})
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.calendar_event_changed", SourceType: "gcal", Kinds: []string{"created", "updated", "cancelled"}}}})
	require.NoError(err)
	count := func() int {
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family = 'msgvault.calendar_event_changed'`).Scan(&count))
		return count
	}
	_, err = s.Full(t.Context())
	require.NoError(err)
	assert.Zero(count())
	delta := timedEvent("live-delta", "Live synthetic meeting")
	m.IncEvents["T1"] = [][]gcal.Event{{delta}}
	m.IncNextToken["T1"] = "T2"
	_, err = s.Incremental(t.Context())
	require.NoError(err)
	assert.Equal(1, count())
	_, err = s.PersistEvent(t.Context(), calendar, delta)
	require.NoError(err)
	assert.Equal(1, count(), "identical write-through redelivery emits nothing")
	delta.Start.DateTime = delta.Start.DateTime.AddDate(0, 0, 1)
	delta.End.DateTime = delta.End.DateTime.AddDate(0, 0, 1)
	_, err = s.PersistEvent(t.Context(), calendar, delta)
	require.NoError(err)
	assert.Equal(2, count())
}
