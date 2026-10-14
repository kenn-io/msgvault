package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogicalMeetingKeysFollowOwnerLinks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("single", "planning@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	first := granolaLinkFixture(t, st, "single")
	_, second := projectionFixture(t, st, "twenty-copy", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	_, unlinked := projectionFixture(t, st, "unlinked", "meeting_json", `{"summary_text":"Planning"}`)
	keys, err := st.LogicalMeetingKeysContext(t.Context(), []int64{event, first, second, unlinked})
	requirements.NoError(err)
	assertions.Equal(map[int64]string{first: "ical:planning@example.org", second: "ical:planning@example.org", unlinked: fmt.Sprintf("msg:%d", unlinked)}, keys)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", event), second))
	keys, err = st.LogicalMeetingKeysContext(t.Context(), []int64{first, second})
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("msg:%d", second), keys[second])
	assertions.Equal("ical:planning@example.org", keys[first])
	_, err = st.LogicalMeetingKeysContext(t.Context(), make([]int64, 101))
	assertions.ErrorContains(err, "100")
}

func TestLogicalMeetingKeysRecurringCopiesPreserveOccurrenceIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.org", linkSeriesRaw)
	calendarLinkFixture(t, st, "primary@example.org", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, first := projectionFixture(t, st, "first-copy", "granola_json", `{"calendar_event":{"calendar_event_id":"series_20270314T100000Z","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "edited-copy", "granola_json", `{"calendar_event":{"calendar_event_id":"edited","scheduled_start_time":"2027-03-14T12:00:00Z"}}`)
	_, third := projectionFixture(t, st, "later-copy", "granola_json", `{"calendar_event":{"calendar_event_id":"series_20270321T100000Z","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
	keys, err := st.LogicalMeetingKeysContext(t.Context(), []int64{first, second, third})
	requirements.NoError(err)
	assertions.Equal("ical:weekly@example.org@20270314T100000Z", keys[first])
	assertions.Equal(keys[first], keys[second], "edited display time keeps the original recurrence key")
	assertions.Equal("ical:weekly@example.org@20270321T100000Z", keys[third])
}
