package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingLinksUnidentifiedExceptionWithCancelledMaster(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	_, exception := calendarFixture(t, st, "exception", `{"id":"edited","status":"confirmed","recurringEventId":"series","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T12:00:00Z"}}`)
	ref := OccurrenceRef{MessageID: exception}
	start := time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	targets := []MeetingLinkRef{{Kind: "evt", OccurrenceRef: ref}, {Kind: "msg", MessageID: note}}
	for _, target := range targets {
		suggestions, err := st.SuggestMeetingLinksContext(t.Context(), target, 10)
		requirements.NoError(err)
		requirements.Len(suggestions.Suggestions, 1, "confirmed exception is suggested in both directions")
		assertions.Equal(formatMeetingOccurrence(ref), suggestions.Suggestions[0].Ref)
	}
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(`{"id":"series","status":"cancelled"}`), "gcal_json"))

	page, err := st.ListCalendarOccurrencesContext(t.Context(), start, end)
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	listed := page.Occurrences[0]
	assertions.Equal(formatMeetingOccurrence(ref), listed.Ref)
	assertions.Equal("cancelled", listed.Status)
	assertions.True(start.Equal(listed.Start))
	assertions.True(end.Equal(listed.End))

	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	budget := calrecur.RequestReadBudget
	resolved, err := resolveMeetingOccurrence(ctx, st.meetingReader(ctx), ref, &budget)
	release()
	requirements.NoError(err)
	assertions.Equal("cancelled", resolved.Row.Event.Status)
	assertions.Equal(ref, resolved.Ref)
	assertions.True(start.Equal(resolved.Start))
	assertions.True(end.Equal(resolved.End))
	assertions.Equal(listed.Ref, linkedMeetingRefs(t, st, note)[note])

	for _, target := range targets {
		suggestions, err := st.SuggestMeetingLinksContext(t.Context(), target, 10)
		requirements.NoError(err)
		assertions.Empty(suggestions.Suggestions, "cancelled exception is excluded for %s", target.Kind)
	}
}

func TestMeetingLinksUnidentifiedExceptionsWithSameUIDKeepDistinctIdentity(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, _ = calendarFixture(t, st, "master", linkSeriesRaw)
	_, first := calendarFixture(t, st, "first", `{"id":"edit-a","iCalUID":"weekly@example.org","recurringEventId":"series","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, second := calendarFixture(t, st, "second", `{"id":"edit-b","iCalUID":"weekly@example.org","recurringEventId":"series","start":{"dateTime":"2027-03-14T14:00:00Z"},"end":{"dateTime":"2027-03-14T15:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	firstRef, secondRef := OccurrenceRef{MessageID: first}, OccurrenceRef{MessageID: second}

	requirements.NoError(st.LinkMeetingContext(t.Context(), note, firstRef))
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, secondRef))
	links, err := st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 2)
	assertions.Equal(formatMeetingOccurrence(firstRef), links[0].Ref)
	assertions.Equal("2027-03-14T12:00:00Z", links[0].Start.Format(time.RFC3339))
	assertions.Equal(formatMeetingOccurrence(secondRef), links[1].Ref)
	assertions.Equal("2027-03-14T14:00:00Z", links[1].Start.Format(time.RFC3339))

	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, firstRef))
	links, err = st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(formatMeetingOccurrence(secondRef), links[0].Ref)

	requirements.NoError(st.LinkMeetingContext(t.Context(), note, firstRef))
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, secondRef))
	links, err = st.MeetingLinksContext(t.Context(), note)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal(formatMeetingOccurrence(firstRef), links[0].Ref)
}

func TestMeetingLinksAllDayExceptionWithTombstoneMaster(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, edit := calendarFixture(t, st, "edit", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T12:00:00Z"}}`)
	ref := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(`{"id":"weekly","status":"cancelled"}`), "gcal_json"))
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	listed := page.Occurrences[0]
	assertions.Equal(formatMeetingOccurrence(ref), listed.Ref)
	assertions.Equal("cancelled", listed.Status)
	assertions.True(from.Equal(listed.Start))
	assertions.True(from.AddDate(0, 0, 1).Equal(listed.End))
	t.Run("saved link", func(t *testing.T) {
		requirements, assertions := require.New(t), assert.New(t)
		links, err := st.MeetingLinksContext(t.Context(), note)
		requirements.NoError(err)
		requirements.Len(links, 1)
		assertions.Equal(listed.Ref, links[0].Ref)
		assertions.True(listed.Start.Equal(links[0].Start))
		assertions.True(listed.End.Equal(links[0].End))
	})
	for _, target := range []MeetingLinkRef{{Kind: "evt", OccurrenceRef: ref}, {Kind: "msg", MessageID: note}} {
		t.Run("suggest/"+target.Kind, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			suggestions, err := st.SuggestMeetingLinksContext(t.Context(), target, 10)
			requirements.NoError(err)
			assertions.Empty(suggestions.Suggestions, "cancelled meetings are excluded in both directions")
		})
	}
	for _, target := range []OccurrenceRef{ref, {MessageID: edit}} {
		t.Run(fmt.Sprintf("link and unlink/%d", target.MessageID), func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, target))
			links, err := st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			requirements.Len(links, 1)
			assertions.Equal(listed.Ref, links[0].Ref)
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, target))
			links, err = st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			assertions.Empty(links)
		})
	}
}

func TestMeetingLinksAllDayExceptionOverSeriesBudget(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"daily","iCalUID":"daily@example.org","start":{"date":"1800-01-01","timeZone":"America/New_York"},"end":{"date":"1800-01-02"},"recurrence":["RRULE:FREQ=DAILY"]}`)
	_, edit := calendarFixture(t, st, "edit", `{"id":"edited","iCalUID":"daily@example.org","recurringEventId":"daily","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-15T02:00:00Z"}}`)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	listed := page.Occurrences[0]
	assertions.Equal([]CalendarUnexpandable{{MessageID: master, Reason: calrecur.ErrBudgetExceeded.Error()}}, page.Unexpandable)
	assertions.Equal("2027-03-14T05:00:00Z", listed.Start.UTC().Format(time.RFC3339))
	assertions.Equal("2027-03-15T04:00:00Z", listed.End.UTC().Format(time.RFC3339))
	for _, ref := range []OccurrenceRef{{MessageID: master, Key: "20270314"}, {MessageID: edit}} {
		requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
		links, err := st.MeetingLinksContext(t.Context(), note)
		requirements.NoError(err)
		requirements.Len(links, 1)
		assertions.True(listed.Start.Equal(links[0].Start))
		assertions.True(listed.End.Equal(links[0].End))
	}
	for _, target := range []MeetingLinkRef{{Kind: "evt", MessageID: master, Key: "20270314"}, {Kind: "msg", MessageID: note}} {
		suggestions, err := st.SuggestMeetingLinksContext(t.Context(), target, 10)
		requirements.NoError(err)
		if assertions.Len(suggestions.Suggestions, 1) {
			assertions.Equal(listed.Ref, suggestions.Suggestions[0].Ref)
			assertions.Equal(note, suggestions.Suggestions[0].MemberMessageID)
			assertions.Equal(30, suggestions.Suggestions[0].OverlapMinutes)
		}
	}
}
