package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingRecordCancelledMasterSeriesInstanceDiscovery(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"series","status":"cancelled","recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, copyID := calendarFixture(t, st, "copy", strings.Replace(linkSeriesRaw, `"id":"series"`, `"id":"copy-series"`, 1), "copy@example.com")
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	require.Equal(t, fmt.Sprintf("evt:%d@20270314T100000Z", master), linkedMeetingRefs(t, st, note)[note])

	for _, ref := range []MeetingLinkRef{
		{Kind: "evt", MessageID: copyID, Key: "20270314T100000Z"},
		{Kind: "evt", MessageID: exception},
	} {
		t.Run(fmt.Sprintf("event-%d", ref.MessageID), func(t *testing.T) {
			record, err := st.MeetingRecordContext(t.Context(), ref)
			require.NoError(t, err)
			assert.Equal(t, []int64{note}, record.MessageIDs)
		})
	}
}
