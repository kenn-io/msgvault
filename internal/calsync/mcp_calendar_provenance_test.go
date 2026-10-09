package calsync

import (
	"encoding/json"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPCalendarSequenceIncreaseFromDefaultZeroEmitsUpdate(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	m := gcal.NewMockAPI()
	calendar := gcal.Calendar{ID: "primary", AccessRole: "owner"}
	m.Calendars = []gcal.Calendar{calendar}
	prior := timedEvent("sequence-only", "Synthetic meeting")
	m.FullEvents["primary"] = [][]gcal.Event{{prior}}
	m.FullSyncToken["primary"] = "T1"
	s, st := newSyncer(t, m, Options{})
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled:   true,
		Principal: "owner",
		Capabilities: []store.MCPEventCapability{{
			Family: "msgvault.calendar_event_changed", SourceType: "gcal",
			Kinds: []string{"created", "updated", "cancelled"},
		}},
	})
	require.NoError(err)

	_, err = s.Full(t.Context())
	require.NoError(err)
	source := primarySource(t, st)
	priorRow, found := getMsg(t, st, source.ID, prior.ID)
	require.True(found)
	var priorMetadata eventMetadata
	require.NoError(json.Unmarshal([]byte(priorRow.metadata.String), &priorMetadata))
	require.NotNil(priorMetadata.Sequence, "a complete event uses the iCalendar default sequence")
	assert.Zero(*priorMetadata.Sequence)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family = ?`), "msgvault.calendar_event_changed").Scan(&count))
	assert.Zero(count, "full sync must remain muted")

	updated := prior
	updated.Sequence = 1
	m.IncEvents["T1"] = [][]gcal.Event{{updated}}
	m.IncNextToken["T1"] = "T2"
	_, err = s.Incremental(t.Context())
	require.NoError(err)

	rows, err := st.DB().Query(st.Rebind(`SELECT kind, data FROM mcp_event_log WHERE family = ? ORDER BY seq`), "msgvault.calendar_event_changed")
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(rows.Close()) })
	var kinds []string
	var payloadJSON string
	for rows.Next() {
		var kind string
		require.NoError(rows.Scan(&kind, &payloadJSON))
		kinds = append(kinds, kind)
	}
	require.NoError(rows.Err())
	require.Equal([]string{"updated"}, kinds)
	var payload struct {
		Sequence *int `json:"sequence"`
	}
	require.NoError(json.Unmarshal([]byte(payloadJSON), &payload))
	require.NotNil(payload.Sequence)
	assert.Equal(1, *payload.Sequence)

	_, err = s.PersistEvent(t.Context(), calendar, updated)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family = ?`), "msgvault.calendar_event_changed").Scan(&count))
	assert.Equal(1, count, "identical write-through redelivery must not append another occurrence")
}

func TestMCPCalendarMetadataKeepsSparseCancellationSequenceUnknown(t *testing.T) {
	assert := Assert.New(t)

	metadata := buildMetadata(gcal.Event{Status: gcal.StatusCancelled}, gcal.Calendar{}, "")
	assert.Nil(metadata.Sequence)
}

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
