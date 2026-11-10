package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingSplitBeforeMasterArrives(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, exception := calendarFixture(t, st, "exception", `{"id":"series_20270314T100000Z","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	require.NoError(t, st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	calendarFixture(t, st, "master", linkSeriesRaw)
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
	require.NoError(t, err)
	assert.Empty(t, record.Links)
}

func TestMeetingRecordIgnoresOtherRecurringDates(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := strings.ReplaceAll(strings.ReplaceAll(linkSeriesRaw, "2027-03-07", "2024-01-01"), "FREQ=WEEKLY;COUNT=6", "FREQ=DAILY;COUNT=1500")
	_, master := calendarFixture(t, st, "master", raw)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2024-01-01T10:00:00Z"}}`)
	start := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	for i := 1; i < 1100; i++ {
		at := start.AddDate(0, 0, i).Format(time.RFC3339)
		projectionFixture(t, st, fmt.Sprintf("daily-%d", i), "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, at))
	}
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: master, Key: "20240101T100000Z"})
	requirements.NoError(err)
	assertions.Equal([]int64{note}, record.MessageIDs)
}
