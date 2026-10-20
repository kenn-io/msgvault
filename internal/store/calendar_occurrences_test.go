package store

import (
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestListCalendarOccurrences(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"],"attendees":[{"email":"guest@example.com","responseStatus":"accepted"}]}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"status":"cancelled"}`)
	_, moved := calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"weekly","status":"confirmed","originalStartTime":{"dateTime":"2027-03-16T17:00:00Z"},"start":{"dateTime":"2027-04-02T19:00:00Z"},"end":{"dateTime":"2027-04-02T20:00:00Z"}}`)
	_, err := st.db.Exec(`UPDATE messages SET subject = '' WHERE id = ?`, cancelled)
	requirements.NoError(err)
	_, single := calendarFixture(t, st, "single", calendarRaw)
	from, err := time.Parse(time.RFC3339, "2027-03-01T00:00:00Z")
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 3)
	assertions.Empty(page.Unexpandable)
	assertions.Equal(master, page.Occurrences[0].MessageID)
	assertions.Equal(fmt.Sprintf("evt:%d@20270302T170000Z", master), page.Occurrences[0].Ref)
	assertions.Equal(single, page.Occurrences[1].MessageID)
	assertions.Equal(fmt.Sprintf("evt:%d", single), page.Occurrences[1].Ref)
	assertions.Equal("cancelled", page.Occurrences[2].Status)
	assertions.Equal(master, page.Occurrences[2].SeriesMessageID)
	assertions.Equal(cancelled, page.Occurrences[2].MessageID)
	assertions.Equal("Example event", page.Occurrences[2].Subject)
	assertions.Equal(page.Occurrences[0].Attendees, page.Occurrences[2].Attendees)
	assertions.Equal("accepted", page.Occurrences[0].Attendees[0].RSVP)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from.AddDate(0, 1, 0), from.AddDate(0, 2, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
	assertions.Equal(moved, page.Occurrences[0].MessageID)
	assertions.Equal(fmt.Sprintf("evt:%d@20270316T170000Z", master), page.Occurrences[0].Ref)
	assertions.Equal("2027-04-02T19:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
}
func TestListCalendarOccurrencesStandaloneMovedToRetainedOriginal(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-02T09:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2027-03-02T10:00:00Z","timeZone":"UTC"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, edit := calendarFixture(t, st, "edit", `{"id":"edited","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"},"start":{"dateTime":"2027-03-10T12:00:00Z"},"end":{"dateTime":"2027-03-10T13:00:00Z"}}`)
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	require.NoError(err)
	require.Len(page.Occurrences, 3)

	// Sync the recurrence removal, then a move of the standalone meeting.
	require.NoError(st.UpsertMessageRawWithFormat(master, []byte(`{"id":"weekly","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"}}`), "gcal_json"))
	require.NoError(st.UpsertMessageRawWithFormat(master, []byte(`{"id":"weekly","start":{"dateTime":"2027-03-09T09:00:00Z"},"end":{"dateTime":"2027-03-09T10:00:00Z"}}`), "gcal_json"))
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	require.NoError(err)
	assert.Empty(page.Unexpandable)
	require.Len(page.Occurrences, 2, "both the current standalone meeting and the retained edit must remain visible")
	assert.Equal(master, page.Occurrences[0].MessageID)
	assert.Zero(page.Occurrences[0].SeriesMessageID)
	assert.Equal("2027-03-09T09:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	assert.Equal(edit, page.Occurrences[1].MessageID)
	assert.Equal(master, page.Occurrences[1].SeriesMessageID)
	assert.Equal("2027-03-10T12:00:00Z", page.Occurrences[1].Start.Format(time.RFC3339))
}

func TestListCalendarOccurrencesMasterChangesValueType(t *testing.T) {
	for _, tc := range []struct {
		name, oldStart, oldEnd, original, editStart, editEnd, newStart, newEnd string
		wantStarts                                                             []string
	}{
		{
			name:     "all-day to timed",
			oldStart: `{"date":"2027-03-02"}`, oldEnd: `{"date":"2027-03-03"}`,
			original:  `{"date":"2027-03-09"}`,
			editStart: `{"date":"2027-03-10"}`, editEnd: `{"date":"2027-03-11"}`,
			newStart: `{"dateTime":"2027-03-02T09:00:00Z","timeZone":"UTC"}`, newEnd: `{"dateTime":"2027-03-02T10:00:00Z","timeZone":"UTC"}`,
			wantStarts: []string{"2027-03-02T09:00:00Z", "2027-03-09T09:00:00Z", "2027-03-10T00:00:00Z", "2027-03-16T09:00:00Z"},
		},
		{
			name:     "timed to all-day",
			oldStart: `{"dateTime":"2027-03-02T09:00:00Z","timeZone":"UTC"}`, oldEnd: `{"dateTime":"2027-03-02T10:00:00Z","timeZone":"UTC"}`,
			original:  `{"dateTime":"2027-03-09T09:00:00Z"}`,
			editStart: `{"dateTime":"2027-03-10T12:00:00Z"}`, editEnd: `{"dateTime":"2027-03-10T13:00:00Z"}`,
			newStart: `{"date":"2027-03-02"}`, newEnd: `{"date":"2027-03-03"}`,
			wantStarts: []string{"2027-03-02T00:00:00Z", "2027-03-09T00:00:00Z", "2027-03-10T12:00:00Z", "2027-03-16T00:00:00Z"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"weekly","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`, tc.oldStart, tc.oldEnd))
			_, edit := calendarFixture(t, st, "edit", fmt.Sprintf(`{"id":"edited","recurringEventId":"weekly","originalStartTime":%s,"start":%s,"end":%s}`, tc.original, tc.editStart, tc.editEnd))
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			require.NoError(err)
			require.Len(page.Occurrences, 3)
			assert.Empty(page.Unexpandable)

			// A later master sync leaves the archived edit's immutable original intact.
			require.NoError(st.UpsertMessageRawWithFormat(master, []byte(fmt.Sprintf(`{"id":"weekly","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`, tc.newStart, tc.newEnd)), "gcal_json"))
			page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			require.NoError(err)
			assert.Empty(page.Unexpandable)
			var starts []string
			var ids []int64
			for _, occurrence := range page.Occurrences {
				starts = append(starts, occurrence.Start.Format(time.RFC3339))
				ids = append(ids, occurrence.MessageID)
				assert.Equal(master, occurrence.SeriesMessageID)
			}
			assert.Equal(tc.wantStarts, starts)
			assert.Equal([]int64{master, master, edit, master}, ids)
		})
	}
}

func TestListCalendarOccurrencesMasterChangesValueTypeSparseCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, oldStart, oldEnd, original, newStart, newEnd string
		wantStarts, wantEnds                               []string
		wantAllDay                                         bool
	}{
		{
			name:     "all-day to timed",
			oldStart: `{"date":"2027-03-02"}`, oldEnd: `{"date":"2027-03-03"}`,
			original: `{"date":"2027-03-09"}`,
			newStart: `{"dateTime":"2027-03-02T09:00:00Z"}`, newEnd: `{"dateTime":"2027-03-02T10:00:00Z"}`,
			wantStarts: []string{"2027-03-02T09:00:00Z", "2027-03-09T09:00:00Z", "2027-03-16T09:00:00Z"},
			wantEnds:   []string{"2027-03-02T10:00:00Z", "2027-03-09T10:00:00Z", "2027-03-16T10:00:00Z"},
		},
		{
			name:     "timed to all-day",
			oldStart: `{"dateTime":"2027-03-02T09:00:00Z"}`, oldEnd: `{"dateTime":"2027-03-02T10:00:00Z"}`,
			original: `{"dateTime":"2027-03-09T09:00:00Z"}`,
			newStart: `{"date":"2027-03-02"}`, newEnd: `{"date":"2027-03-03"}`,
			wantStarts: []string{"2027-03-02T00:00:00Z", "2027-03-09T00:00:00Z", "2027-03-16T00:00:00Z"},
			wantEnds:   []string{"2027-03-03T00:00:00Z", "2027-03-10T00:00:00Z", "2027-03-17T00:00:00Z"},
			wantAllDay: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"weekly","status":"confirmed","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`, tc.oldStart, tc.oldEnd))
			_, cancelled := calendarFixture(t, st, "cancelled", fmt.Sprintf(`{"id":"cancelled","recurringEventId":"weekly","originalStartTime":%s,"status":"cancelled"}`, tc.original))
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			require.NoError(err)
			require.Len(page.Occurrences, 3)
			assert.Equal(cancelled, page.Occurrences[1].MessageID)
			assert.Equal("cancelled", page.Occurrences[1].Status)
			assert.Empty(page.Unexpandable)

			require.NoError(st.UpsertMessageRawWithFormat(master, []byte(fmt.Sprintf(`{"id":"weekly","status":"confirmed","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`, tc.newStart, tc.newEnd)), "gcal_json"))
			page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			require.NoError(err)
			assert.Equal([]CalendarUnexpandable{{MessageID: cancelled, Reason: "exception original type differs from series and event bounds are missing"}}, page.Unexpandable)
			require.Len(page.Occurrences, 3, "the current series must remain visible after its type changes")
			for i, occurrence := range page.Occurrences {
				assert.Equal(master, occurrence.MessageID)
				assert.Equal(master, occurrence.SeriesMessageID)
				assert.Equal("confirmed", occurrence.Status)
				assert.Equal(tc.wantAllDay, occurrence.AllDay)
				assert.Equal(tc.wantStarts[i], occurrence.Start.Format(time.RFC3339))
				assert.Equal(tc.wantEnds[i], occurrence.End.Format(time.RFC3339))
			}
		})
	}
}

func TestListCalendarOccurrencesUnidentifiedExceptions(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, edit := calendarFixture(t, st, "edit", `{"id":"edit","recurringEventId":"weekly","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled"}`)
	_, unidentified := calendarFixture(t, st, "unidentified", `{"id":"unidentified","recurringEventId":"weekly","status":"cancelled"}`)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 21))
	require.NoError(err)
	assert.ElementsMatch([]CalendarUnexpandable{
		{MessageID: edit, Reason: "exception has no original start"},
		{MessageID: unidentified, Reason: "exception has no original start"},
	}, page.Unexpandable)
	require.Len(page.Occurrences, 4)
	for i, want := range []struct {
		id                 int64
		start, end, status string
	}{
		{master, "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z", "confirmed"},
		{edit, "2027-03-14T12:00:00Z", "2027-03-14T13:00:00Z", "confirmed"},
		{cancelled, "2027-03-21T10:00:00Z", "2027-03-21T11:00:00Z", "cancelled"},
		{master, "2027-03-28T10:00:00Z", "2027-03-28T11:00:00Z", "confirmed"},
	} {
		got := page.Occurrences[i]
		assert.Equal(want.id, got.MessageID)
		assert.Equal(master, got.SeriesMessageID)
		assert.Equal(want.start, got.Start.Format(time.RFC3339))
		assert.Equal(want.end, got.End.Format(time.RFC3339))
		assert.Equal(want.status, got.Status)
	}
}

func TestListCalendarOccurrencesUnexpandable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "unsupported", `{"id":"unsupported","start":{"date":"2027-03-01"},"end":{"date":"2027-03-02"},"recurrence":["RRULE:FREQ=HOURLY"]}`)
	from, err := time.Parse(time.DateOnly, "2027-03-01")
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences)
	requirements.Len(page.Unexpandable, 1)
	assertions.Equal(id, page.Unexpandable[0].MessageID)
}

func TestListCalendarOccurrencesUnidentifiedExceptionsWithoutRecurrence(t *testing.T) {
	for _, masterPresent := range []bool{true, false} {
		t.Run(fmt.Sprintf("master=%t", masterPresent), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var master int64
			if masterPresent {
				_, master = calendarFixture(t, st, "master", `{"id":"single","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
			}
			_, edit := calendarFixture(t, st, "edit", `{"id":"edit","recurringEventId":"single","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
			_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"single","status":"cancelled"}`)
			from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			require.NoError(err)
			assert.ElementsMatch([]CalendarUnexpandable{
				{MessageID: edit, Reason: "exception has no original start"},
				{MessageID: cancelled, Reason: "exception has no original start"},
			}, page.Unexpandable)
			if masterPresent {
				require.Len(page.Occurrences, 2)
				assert.Equal(master, page.Occurrences[0].MessageID)
				page.Occurrences = page.Occurrences[1:]
			}
			require.Len(page.Occurrences, 1)
			assert.Equal(edit, page.Occurrences[0].MessageID)
			assert.Equal(master, page.Occurrences[0].SeriesMessageID)
			assert.Equal("2027-03-14T12:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
			assert.Equal("2027-03-14T13:00:00Z", page.Occurrences[0].End.Format(time.RFC3339))
		})
	}
}

func TestListCalendarOccurrencesOffsetFreeTime(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "offset-free", `{"id":"offset-free","start":{"dateTime":"2027-03-02T09:00:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-02T10:00:00","timeZone":"America/New_York"}}`)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Empty(page.Unexpandable)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(id, page.Occurrences[0].MessageID)
	assertions.Equal("2027-03-02T14:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	assertions.Equal("2027-03-02T15:00:00Z", page.Occurrences[0].End.Format(time.RFC3339))
}

func TestListCalendarOccurrencesYearlyByMonth(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := calendarFixture(t, st, "yearly", `{"id":"yearly","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYMONTH=3"]}`)
	from := time.Date(2028, time.March, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	require.NoError(err)
	assert.Empty(page.Unexpandable)
	require.Len(page.Occurrences, 1)
	assert.Equal(id, page.Occurrences[0].MessageID)
	assert.Equal("2028-03-02T17:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
}

func TestListCalendarOccurrencesUndecodableEvent(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	var expected []CalendarUnexpandable
	for _, tt := range []struct {
		name, raw, reason string
	}{
		{"unknown-zone", `{"id":"unknown-zone","start":{"date":"2027-03-02","timeZone":"Unknown/Zone"},"end":{"date":"2027-03-03","timeZone":"Unknown/Zone"}}`, "event data could not be decoded"},
		{"corrupt", `{"id":`, "event data could not be decoded"},
		{"oversized", `{"id":"oversized","description":"` + strings.Repeat("a", maxMeetingRawBytes) + `"}`, "event data could not be decoded"},
		{"missing-bounds", `{"id":"missing-bounds"}`, "missing event bounds"},
	} {
		_, id := calendarFixture(t, st, tt.name, tt.raw)
		expected = append(expected, CalendarUnexpandable{MessageID: id, Reason: tt.reason})
	}
	assertions, requirements := assert.New(t), require.New(t)
	from := time.Date(2027, time.March, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences)
	assertions.Equal(expected, page.Unexpandable)
}

func TestListCalendarOccurrencesRequestBudgetKeepsCompletedResults(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, completed := calendarFixture(t, st, "completed", `{"id":"completed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`)
	_, completedException := calendarFixture(t, st, "completed-exception", `{"id":"completed-exception","recurringEventId":"completed","originalStartTime":{"dateTime":"2027-03-03T17:00:00Z"},"status":"cancelled"}`)
	var interrupted int64
	for i := range calrecur.RequestReadBudget / calrecur.SeriesReadBudget {
		_, interrupted = calendarFixture(t, st, fmt.Sprintf("long-%d", i), fmt.Sprintf(`{"id":"long-%d","start":{"dateTime":"1800-01-01T17:00:00Z"},"end":{"dateTime":"1800-01-01T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, i))
	}
	_, interruptedException := calendarFixture(t, st, "interrupted-exception", `{"id":"interrupted-exception","recurringEventId":"long-9","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"},"status":"cancelled"}`)
	_, later := calendarFixture(t, st, "later", `{"id":"later","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`)
	_, laterException := calendarFixture(t, st, "later-exception", `{"id":"later-exception","recurringEventId":"later","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"},"status":"cancelled"}`)
	_, single := calendarFixture(t, st, "single", calendarRaw)
	// Include the long series' early occurrences to catch partial expansion leaks.
	from := time.Date(1800, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, to)
	requirements.NoError(err)
	var ids []int64
	for _, occurrence := range page.Occurrences {
		ids = append(ids, occurrence.MessageID)
	}
	assertions.ElementsMatch([]int64{completed, completedException, interruptedException, laterException, single}, ids)
	requirements.Len(page.Unexpandable, 11)
	assertions.Equal([]CalendarUnexpandable{
		{MessageID: interrupted, Reason: "request work limit reached; long-running series count their steps from their start"},
		{MessageID: later, Reason: "request work limit reached; long-running series count their steps from their start"},
	}, page.Unexpandable[9:])
}

func TestListCalendarOccurrencesOrphanExceptions(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, complete := calendarFixture(t, st, "orphan", `{"id":"orphan","recurringEventId":"missing","status":"confirmed","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"},"start":{"dateTime":"2027-03-03T17:00:00Z"},"end":{"dateTime":"2027-03-03T18:00:00Z"}}`)
	_, incomplete := calendarFixture(t, st, "sparse-orphan", `{"id":"sparse-orphan","recurringEventId":"missing","status":"cancelled","originalStartTime":{"dateTime":"2027-03-04T17:00:00Z"}}`)
	_, unidentified := calendarFixture(t, st, "unidentified-orphan", `{"id":"unidentified-orphan","recurringEventId":"missing","status":"cancelled"}`)
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(complete, page.Occurrences[0].MessageID)
	assertions.Zero(page.Occurrences[0].SeriesMessageID)
	assertions.Equal("2027-03-03T17:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	assertions.ElementsMatch([]CalendarUnexpandable{{MessageID: incomplete, Reason: "missing series master and event bounds"}, {MessageID: unidentified, Reason: "exception has no original start"}}, page.Unexpandable)
}

func TestListCalendarOccurrencesAllDayQueryZone(t *testing.T) {
	for _, recurring := range []bool{false, true} {
		t.Run(strconv.FormatBool(recurring), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			recurrence := ""
			if recurring {
				recurrence = `,"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]`
			}
			data, id := calendarFixture(t, st, "all-day", `{"id":"all-day","start":{"date":"2027-03-02"},"end":{"date":"2027-03-03"}`+recurrence+`}`)
			requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"time_zone":"Pacific/Auckland"}`))
			zone, err := time.LoadLocation("America/Los_Angeles")
			requirements.NoError(err)
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, zone)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			assertions.Empty(page.Occurrences, "March 2 is outside March 1")
			from = from.AddDate(0, 0, 1)
			page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(id, page.Occurrences[0].MessageID)
			assertions.Equal("2027-03-02", page.Occurrences[0].StartDate)
		})
	}
}

func TestListCalendarOccurrencesAllDayPartialWindow(t *testing.T) {
	for _, tc := range []struct {
		name, endDate, from, to string
		want                    int
	}{
		{"inside", "2027-03-03", "2027-03-02T09:00:00Z", "2027-03-02T17:00:00Z", 1},
		{"end midnight", "2027-03-03", "2027-03-01T09:00:00Z", "2027-03-02T00:00:00Z", 0},
		{"start midday", "2027-03-03", "2027-03-02T17:00:00Z", "2027-03-03T00:00:00Z", 1},
		{"after", "2027-03-03", "2027-03-03T00:00:00Z", "2027-03-03T17:00:00Z", 0},
		{"multi day", "2027-03-05", "2027-03-04T09:00:00-05:00", "2027-03-04T17:00:00-05:00", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "partial", fmt.Sprintf(`{"id":"partial","start":{"date":"2027-03-02"},"end":{"date":%q}}`, tc.endDate))
			from, err := time.Parse(time.RFC3339, tc.from)
			requirements.NoError(err)
			to, err := time.Parse(time.RFC3339, tc.to)
			requirements.NoError(err)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, to)
			requirements.NoError(err)
			assert.Len(t, page.Occurrences, tc.want)
		})
	}
}

func TestListCalendarOccurrencesSkippedStartDate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "skipped-start", `{"id":"skipped-start","start":{"date":"2011-12-30","timeZone":"Pacific/Apia"},"end":{"date":"2011-12-31","timeZone":"Pacific/Apia"},"recurrence":["RRULE:FREQ=DAILY;COUNT=3","RDATE;VALUE=DATE:20111229,20111230"]}`)
	from := time.Date(2011, 12, 28, 0, 0, 0, 0, time.UTC)
	to := time.Date(2012, 1, 4, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, to)
	requirements.NoError(err)
	assertions.Empty(page.Unexpandable)
	var dates []string
	for _, o := range page.Occurrences {
		dates = append(dates, o.StartDate)
	}
	assertions.Equal([]string{"2011-12-29", "2011-12-31", "2012-01-01", "2012-01-02"}, dates)
}

func TestListCalendarOccurrencesMultiDayExceptions(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		for _, moved := range []bool{false, true} {
			t.Run(fmt.Sprintf("allDay=%t/moved=%t", allDay, moved), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				start, end := `{"dateTime":"2027-03-01T09:00:00Z"}`, `{"dateTime":"2027-03-08T09:00:00Z"}`
				newStart, newEnd := `{"dateTime":"2027-04-01T09:00:00Z"}`, `{"dateTime":"2027-04-08T09:00:00Z"}`
				if allDay {
					start, end = `{"date":"2027-03-01"}`, `{"date":"2027-03-08"}`
					newStart, newEnd = `{"date":"2027-04-01"}`, `{"date":"2027-04-08"}`
				}
				_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"weekly","status":"confirmed","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, start, end))
				fields := `"status":"cancelled"`
				if moved {
					fields = fmt.Sprintf(`"status":"confirmed","start":%s,"end":%s`, newStart, newEnd)
				}
				_, exception := calendarFixture(t, st, "exception", fmt.Sprintf(`{"id":"exception","recurringEventId":"weekly","originalStartTime":%s,%s}`, start, fields))
				from := time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				assertions.Empty(page.Unexpandable)
				if moved {
					assertions.Empty(page.Occurrences, "the week-long occurrence moved to April")
					return
				}
				requirements.Len(page.Occurrences, 1)
				assertions.Equal(exception, page.Occurrences[0].MessageID)
				assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
				assertions.Equal("cancelled", page.Occurrences[0].Status)
			})
		}
	}
}

func TestListCalendarOccurrencesSparseCancellationZone(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, original, wantStart, wantEnd string
	}{
		{"all day", `{"date":"2027-03-02","timeZone":"America/New_York"}`, `{"date":"2027-03-03","timeZone":"America/New_York"}`, `{"date":"2027-03-09"}`, "2027-03-09T05:00:00Z", "2027-03-10T05:00:00Z"},
		{"timed", `{"dateTime":"2027-03-02T09:00:00-05:00","timeZone":"America/New_York"}`, `{"dateTime":"2027-03-02T10:00:00-05:00","timeZone":"America/New_York"}`, `{"dateTime":"2027-03-09T09:00:00-05:00"}`, "2027-03-09T14:00:00Z", "2027-03-09T15:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"weekly","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, tc.start, tc.end))
			_, cancelled := calendarFixture(t, st, "cancelled", fmt.Sprintf(`{"id":"cancelled","recurringEventId":"weekly","originalStartTime":%s,"status":"cancelled"}`, tc.original))
			requirements.NoError(st.UpdateSourceSyncConfig(data.Message.SourceID, `{"time_zone":"Asia/Tokyo"}`))
			from := time.Date(2027, 3, 9, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			assertions.Empty(page.Unexpandable)
			requirements.Len(page.Occurrences, 1)
			o := page.Occurrences[0]
			assertions.Equal(cancelled, o.MessageID)
			assertions.Equal(master, o.SeriesMessageID)
			assertions.Equal("cancelled", o.Status)
			assertions.Equal(tc.wantStart, o.Start.UTC().Format(time.RFC3339))
			assertions.Equal(tc.wantEnd, o.End.UTC().Format(time.RFC3339))
		})
	}
}

func TestListCalendarOccurrencesUnexpandableKeepsEditedExceptions(t *testing.T) {
	for _, recurrence := range []string{"RRULE:FREQ=HOURLY", "RRULE:FREQ=DAILY"} {
		t.Run(recurrence, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"master","status":"confirmed","start":{"dateTime":"1800-01-01T17:00:00Z"},"end":{"dateTime":"1800-01-01T18:00:00Z"},"recurrence":[%q]}`, recurrence))
			_, edited := calendarFixture(t, st, "edited", `{"id":"edited","recurringEventId":"master","status":"confirmed","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"},"start":{"dateTime":"2027-03-03T19:00:00Z"},"end":{"dateTime":"2027-03-03T20:00:00Z"}}`)
			calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"master","status":"cancelled","originalStartTime":{"dateTime":"2027-03-04T17:00:00Z"}}`)
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			require.NoError(err)
			require.Len(page.Unexpandable, 1)
			assert.Equal(master, page.Unexpandable[0].MessageID)
			require.Len(page.Occurrences, 1)
			assert.Equal(edited, page.Occurrences[0].MessageID)
			assert.Equal(master, page.Occurrences[0].SeriesMessageID)
			assert.Equal("2027-03-03T19:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
			assert.Equal("2027-03-03T20:00:00Z", page.Occurrences[0].End.Format(time.RFC3339))
		})
	}
}

func TestUnsupportedCalendarFallbackUsesNewestExceptionSnapshot(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=HOURLY"]}`)
	firstRaw := `{"id":"instance","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	secondRaw := strings.Replace(firstRaw, `"2027-03-14T12:00:00Z"`, `"2027-03-14T14:00:00Z"`, 1)
	secondRaw = strings.Replace(secondRaw, `"2027-03-14T13:00:00Z"`, `"2027-03-14T15:00:00Z"`, 1)
	_, first := calendarFixture(t, st, "instance", firstRaw)
	_, second := calendarFixture(t, st, "series|2027-03-14T10:00:00Z", secondRaw)
	now := time.Now().UTC()
	_, err := st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, now, first)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE calendar_events SET snapshot_updated_at = ? WHERE message_id = ?`, now.Add(-time.Minute), second)
	requirements.NoError(err)

	rows := calendarRows(t, st, master, first, second)
	byID := make(map[int64]CalendarEventRow, len(rows))
	for _, row := range rows {
		byID[row.MessageID] = row
	}
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	occurrences, _, reason, err := expandCalendarSeries(ctx, byID[master], []CalendarEventRow{byID[second], byID[first]}, from, from.AddDate(0, 0, 1), 100)
	requirements.NoError(err)
	assertions.Equal("unsupported recurrence", reason)
	requirements.Len(occurrences, 1)
	assertions.Equal(first, occurrences[0].ExceptionRef)
	assertions.Equal(from.Add(12*time.Hour), occurrences[0].Start)
}

func TestListCalendarOccurrencesUnexpandableKeepsCancelledExceptions(t *testing.T) {
	for _, tc := range []struct{ recurrence, reason string }{
		{"RRULE:FREQ=HOURLY", "unsupported recurrence"},
		{"RRULE:FREQ=DAILY", "recurrence generation budget exceeded"},
	} {
		t.Run(tc.recurrence, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"master","start":{"dateTime":"1800-01-01T17:00:00Z"},"end":{"dateTime":"1800-01-01T18:00:00Z"},"recurrence":[%q]}`, tc.recurrence))
			_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"master","status":"cancelled","originalStartTime":{"dateTime":"2027-03-04T17:00:00Z"},"start":{"dateTime":"2027-03-04T19:00:00Z"},"end":{"dateTime":"2027-03-04T20:00:00Z"}}`)
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			requirements.NoError(err)
			requirements.Len(page.Unexpandable, 1)
			assertions.Equal(master, page.Unexpandable[0].MessageID)
			assertions.Equal(tc.reason, page.Unexpandable[0].Reason)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(cancelled, page.Occurrences[0].MessageID)
			assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
			assertions.Equal("cancelled", page.Occurrences[0].Status)
			assertions.Equal("2027-03-04T19:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
			assertions.Equal("2027-03-04T20:00:00Z", page.Occurrences[0].End.Format(time.RFC3339))
		})
	}
}

func TestListCalendarOccurrencesCancelledSinglesWithoutBounds(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 3 {
		calendarFixture(t, st, fmt.Sprintf("cancelled-%d", i), fmt.Sprintf(`{"id":"cancelled-%d","status":"cancelled"}`, i))
	}
	from := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := calendarEventRowsInWindow(t.Context(), st.meetingReader(t.Context()), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Empty(rows, "tombstones must consume no request budget")
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences)
	assertions.Empty(page.Unexpandable)
	_, unknown := calendarFixture(t, st, "unknown", `{"id":"unknown"}`)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Equal([]CalendarUnexpandable{{MessageID: unknown, Reason: "missing event bounds"}}, page.Unexpandable)
}

func TestListCalendarOccurrencesExceptionMessageIDZero(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, master := calendarFixture(t, st, "master", `{"id":"master","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`)
	override := ""
	if st.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	_, err := st.db.Exec(`INSERT INTO messages (id, source_id, conversation_id, source_message_id, message_type, subject, deleted_from_source_at)`+override+` VALUES (0, ?, ?, 'zero', 'calendar_event', 'Edited event', ?)`, data.Message.SourceID, data.Message.ConversationID, time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.NoError(st.UpsertMessageRawWithFormat(0, []byte(`{"id":"edited","recurringEventId":"master","status":"tentative","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"},"start":{"dateTime":"2027-03-03T19:00:00Z"},"end":{"dateTime":"2027-03-03T20:00:00Z"},"attendees":[{"email":"edited@example.com","responseStatus":"declined"}]}`), "gcal_json"))
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	require.NoError(err)
	assert.Empty(page.Unexpandable)
	require.Len(page.Occurrences, 1)
	o := page.Occurrences[0]
	assert.Equal(int64(0), o.MessageID)
	assert.Equal(master, o.SeriesMessageID)
	assert.Equal("Edited event", o.Subject)
	assert.Equal("tentative", o.Status)
	assert.True(o.DeletedFromSource)
	require.Len(o.Attendees, 1)
	assert.Equal("edited@example.com", o.Attendees[0].Email)
	assert.Equal("declined", o.Attendees[0].RSVP)
	assert.Equal("2027-03-03T19:00:00Z", o.Start.Format(time.RFC3339))
}

func TestListCalendarOccurrencesDefaultGoogleStatus(t *testing.T) {
	for _, tc := range []struct {
		name, rawStatus, metadata, want string
	}{
		{"omitted", "", `{"calendar_id":"calendar@example.com"}`, "confirmed"},
		{"raw cancellation", `"status":"cancelled",`, `{"calendar_id":"calendar@example.com"}`, "cancelled"},
		{"metadata cancellation", "", `{"status":"cancelled"}`, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, _ := calendarFixture(t, st, "event", fmt.Sprintf(`{"id":"event",%s"start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"}}`, tc.rawStatus))
			data.Metadata.String = tc.metadata
			_, err := st.PersistMessage(data)
			require.NoError(err)
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
			require.NoError(err)
			require.Len(page.Occurrences, 1)
			body, err := json.Marshal(page)
			require.NoError(err)
			var response generated.CalendarOccurrencePage
			require.NoError(json.Unmarshal(body, &response))
			assert.NoError(response.Validate())
			assert.Equal(tc.want, page.Occurrences[0].Status)
		})
	}
}
