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
		name, bound string
		wantStart   bool
	}{
		{"start-only", `"start":{"dateTime":"2027-03-15T10:00:00Z"}`, true},
		{"end-only", `"end":{"dateTime":"2027-03-15T11:00:00Z"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "owner@example.com")
			_, edit := calendarFixture(t, st, "edit", fmt.Sprintf(`{"id":"edit","recurringEventId":"series","status":"cancelled",%s}`, tc.bound), "owner@example.com")
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: edit})
			require.NoError(t, err)
			require.Len(t, record.Links, 1)
			assert.Equal(t, fmt.Sprintf("evt:%d", edit), record.Links[0].Ref)
			if tc.wantStart {
				require.NotNil(t, record.Links[0].Start)
				assert.Equal(t, time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC), *record.Links[0].Start)
				assert.Nil(t, record.Links[0].End)
			} else {
				assert.Nil(t, record.Links[0].Start)
				require.NotNil(t, record.Links[0].End)
				assert.Equal(t, time.Date(2027, 3, 15, 11, 0, 0, 0, time.UTC), *record.Links[0].End)
			}
			assert.Contains(t, record.Unexpandable, CalendarUnexpandable{MessageID: edit, Reason: "exception has no original start"})
			window := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), window, window.AddDate(0, 0, 1))
			require.NoError(t, err)
			for _, occurrence := range page.Occurrences {
				assert.NotEqual(t, edit, occurrence.MessageID)
			}
		})
	}
}

func TestMeetingRecordUntimedCopyDoesNotJoinSeriesNotesAcrossSources(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	sparse, _ := calendarFixture(t, st, "sparse", `{"id":"series","status":"cancelled"}`, "first@example.com")
	master, _ := calendarFixture(t, st, "master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "second@example.com")
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), sparse.Message.SourceID, `{"calendar_id":"owner@example.com","account_email":"first@example.com","time_zone":"UTC"}`))
	require.NoError(t, st.UpdateSourceSyncConfig(t.Context(), master.Message.SourceID, `{"calendar_id":"OWNER@example.com","account_email":"second@example.com","time_zone":"UTC"}`))
	_, first := projectionFixture(t, st, "week-one", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "week-two", "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"series","scheduled_start_time":"2027-03-21T10:00:00Z"}}`)
	for _, id := range []int64{first, second} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
		require.NoError(t, err)
		assert.Equal(t, []int64{id}, record.MessageIDs)
	}
}
