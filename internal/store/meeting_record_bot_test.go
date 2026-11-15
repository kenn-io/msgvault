package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingRecordPartialEditWithoutOriginalWithMaster(t *testing.T) {
	for _, tc := range []struct {
		name, bound, recurrence string
		wantStart               bool
	}{
		{"supported-start", `"start":{"dateTime":"2027-03-15T10:00:00Z"}`, "RRULE:FREQ=WEEKLY;COUNT=2", true},
		{"supported-end", `"end":{"dateTime":"2027-03-15T11:00:00Z"}`, "RRULE:FREQ=WEEKLY;COUNT=2", false},
		{"unsupported-start", `"start":{"dateTime":"2027-03-15T10:00:00Z"}`, "RRULE:FREQ=YEARLY;BYWEEKNO=20", true},
		{"unsupported-end", `"end":{"dateTime":"2027-03-15T11:00:00Z"}`, "RRULE:FREQ=YEARLY;BYWEEKNO=20", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":[%q]}`, tc.recurrence), "owner@example.com")
			_, edit := calendarFixture(t, st, "edit", fmt.Sprintf(`{"id":"edit","recurringEventId":"series","status":"cancelled",%s}`, tc.bound), "owner@example.com")
			_, note := projectionFixture(t, st, "note", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"edit"}}`)
			for _, seed := range []MeetingLinkRef{{Kind: "evt", MessageID: edit}, {Kind: "msg", MessageID: note}} {
				record, err := st.MeetingRecordContext(t.Context(), seed)
				require.NoError(err)
				require.Len(record.Links, 1)
				assert.Equal(fmt.Sprintf("evt:%d", edit), record.Links[0].Ref)
				if tc.wantStart {
					require.NotNil(record.Links[0].Start)
					assert.Equal(time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC), *record.Links[0].Start)
					assert.Nil(record.Links[0].End)
				} else {
					assert.Nil(record.Links[0].Start)
					require.NotNil(record.Links[0].End)
					assert.Equal(time.Date(2027, 3, 15, 11, 0, 0, 0, time.UTC), *record.Links[0].End)
				}
				assert.Contains(record.Unexpandable, CalendarUnexpandable{MessageID: edit, Reason: "exception has no original start"})
				assert.Equal([]int64{note}, record.MessageIDs)
			}
			window := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 0, 1))
			require.NoError(err)
			for _, occurrence := range page.Occurrences {
				assert.NotEqual(edit, occurrence.MessageID)
			}
		})
	}
}
