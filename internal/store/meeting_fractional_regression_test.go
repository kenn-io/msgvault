package store

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksFractionalOrphanRoundTrip(t *testing.T) {
	for _, fraction := range []string{"", ".123456"} {
		t.Run("seconds"+fraction, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := fmt.Sprintf(`{"id":"instance","iCalUID":"fractional@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00%sZ"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, fraction)
			_, orphan := calendarFixture(t, st, "orphan", raw)
			_, note := projectionFixture(t, st, "fractional-note", "plaud_json", `{}`)
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
			links, err := st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			requirements.Len(links, 1)
			assertions.Equal(fmt.Sprintf("evt:%d", orphan), links[0].Ref)
			assertions.Equal(time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC), links[0].Start)
			assertions.Equal(time.Date(2027, 3, 14, 13, 0, 0, 0, time.UTC), links[0].End)
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: orphan}))
			links, err = st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			assertions.Empty(links)
		})
	}
}

func TestMeetingLinksFractionalExceptionsWithMasterRoundTrip(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","iCalUID":"fractional@example.org","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, first := calendarFixture(t, st, "fractional-first", `{"id":"fractional-first","iCalUID":"fractional@example.org","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.123456","timeZone":"UTC"},"start":{"dateTime":"2027-03-09T19:00:00Z"},"end":{"dateTime":"2027-03-09T20:00:00Z"}}`)
	_, second := calendarFixture(t, st, "weekly|2027-03-09T17:00:00Z", `{"id":"fractional-second","iCalUID":"fractional@example.org","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.654321Z"},"start":{"dateTime":"2027-03-09T21:00:00Z"},"end":{"dateTime":"2027-03-09T22:00:00Z"}}`)
	_, firstNote := projectionFixture(t, st, "fractional-note-first", "plaud_json", `{}`)
	_, secondNote := projectionFixture(t, st, "fractional-note-second", "plaud_json", `{}`)

	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 20))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 5)
	requirements.Len(page.Unexpandable, 2)
	assertions.Equal([]int64{master, master, first, second, master}, []int64{
		page.Occurrences[0].MessageID, page.Occurrences[1].MessageID, page.Occurrences[2].MessageID, page.Occurrences[3].MessageID, page.Occurrences[4].MessageID,
	})
	assertions.Equal("evt:"+strconv.FormatInt(master, 10)+"@20270302T170000Z", page.Occurrences[0].Ref)
	assertions.Equal("evt:"+strconv.FormatInt(master, 10)+"@20270309T170000Z", page.Occurrences[1].Ref)
	assertions.Equal("evt:"+strconv.FormatInt(master, 10)+"@20270316T170000Z", page.Occurrences[4].Ref)
	unexpandableIDs := []int64{page.Unexpandable[0].MessageID, page.Unexpandable[1].MessageID}
	assertions.ElementsMatch([]int64{first, second}, unexpandableIDs)

	for _, tc := range []struct {
		messageID int64
		noteID    int64
		ref       string
		start     time.Time
		end       time.Time
	}{
		{first, firstNote, "evt:" + strconv.FormatInt(first, 10), time.Date(2027, 3, 9, 19, 0, 0, 0, time.UTC), time.Date(2027, 3, 9, 20, 0, 0, 0, time.UTC)},
		{second, secondNote, "evt:" + strconv.FormatInt(second, 10), time.Date(2027, 3, 9, 21, 0, 0, 0, time.UTC), time.Date(2027, 3, 9, 22, 0, 0, 0, time.UTC)},
	} {
		listedIndex := slices.IndexFunc(page.Occurrences, func(occurrence CalendarOccurrence) bool { return occurrence.MessageID == tc.messageID })
		requirements.NotEqual(-1, listedIndex)
		listed := page.Occurrences[listedIndex]
		ref := OccurrenceRef{MessageID: listed.MessageID}
		requirements.NoError(st.LinkMeetingContext(t.Context(), tc.noteID, ref))
		links, err := st.MeetingLinksContext(t.Context(), tc.noteID)
		requirements.NoError(err)
		requirements.Len(links, 1)
		assertions.Equal(tc.ref, links[0].Ref)
		assertions.Equal(tc.start, links[0].Start)
		assertions.Equal(tc.end, links[0].End)
		requirements.NoError(st.UnlinkMeetingContext(t.Context(), tc.noteID, ref))
		links, err = st.MeetingLinksContext(t.Context(), tc.noteID)
		requirements.NoError(err)
		assertions.Empty(links)
	}
}

func testMeetingLinksFractionalAliasResolvesFreshestSnapshotForUnlink(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", linkSeriesRaw)
	_, older := calendarFixture(t, st, "fractional-instance", `{"id":"fractional-instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.123456","timeZone":"UTC"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"updated":"2027-03-14T08:00:00Z"}`)
	_, note := projectionFixture(t, st, "fractional-alias-note", "plaud_json", `{}`)

	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: older}))
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", older), links[0].Ref)
	assertions.Equal(time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC), links[0].Start)

	_, newer := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", `{"id":"fractional-instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.123456Z"},"start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"},"updated":"2027-03-15T08:00:00Z"}`)
	links, err = st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", newer), links[0].Ref)
	assertions.Equal(time.Date(2027, 3, 14, 14, 0, 0, 0, time.UTC), links[0].Start)
	assertions.Equal(time.Date(2027, 3, 14, 15, 0, 0, 0, time.UTC), links[0].End)

	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: newer}))
	links, err = st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	assertions.Empty(links)
}

func testMeetingLinksExceptionWithoutOriginalStartUsesMessageReference(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "master", linkSeriesRaw)
	_, exception := calendarFixture(t, st, "unidentified-exception", `{"id":"unidentified","iCalUID":"weekly@example.org","recurringEventId":"series","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "unidentified-note", "plaud_json", `{}`)

	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", exception), links[0].Ref)
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
	links, err = st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	assertions.Empty(links)
}

func testMeetingSuggestionsExcludeFractionalExceptionOfCancelledSeries(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "cancelled-master", `{"id":"series","status":"cancelled","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	_, exception := calendarFixture(t, st, "fractional-exception", `{"id":"instance","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.123456Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "cancelled-note", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2027-03-14T12:00:00Z","endedAt":"2027-03-14T13:00:00Z"},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)

	byNote, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	requirements.NoError(err)
	assertions.Empty(byNote.Suggestions)
	byEvent, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: exception}, 10)
	requirements.NoError(err)
	assertions.Empty(byEvent.Suggestions)
}

func TestMeetingExceptionWithoutOriginalStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"MeetingLinksExceptionWithoutOriginalStartUsesMessageReference", testMeetingLinksExceptionWithoutOriginalStartUsesMessageReference},
		{"MeetingLinksUnidentifiedExceptionsUseMessageIdentity", testMeetingLinksUnidentifiedExceptionsUseMessageIdentity},
		{"MeetingUnsupportedSeriesExceptionWithoutOriginal", testMeetingUnsupportedSeriesExceptionWithoutOriginal},
	} {
		t.Run(tc.name, tc.run)
	}
}
