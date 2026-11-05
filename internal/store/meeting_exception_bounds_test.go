package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingLinksUnidentifiedExceptionWithCancelledMaster(t *testing.T) {
	t.Helper()
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
	resolved, err := resolveMeetingOccurrence(ctx, st.meetingReader(), ref, &budget)
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

func TestMeetingVersionlessCancellationOverridesVersionedException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", linkSeriesRaw)
	original := `"originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}`
	calendarFixture(t, st, "confirmed", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"series",`+original+`,"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"},"updated":"2027-03-10T12:00:00Z","sequence":7}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","iCalUID":"weekly@example.org","recurringEventId":"series",`+original+`,"status":"cancelled"}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)

	from := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(time.Hour))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(cancelled, page.Occurrences[0].MessageID)
	assertions.Equal(master, page.Occurrences[0].SeriesMessageID)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
	assertions.Equal(from, page.Occurrences[0].Start)
	assertions.Equal(from.Add(time.Hour), page.Occurrences[0].End)

	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	occurrences, err := st.meetingMemberCandidates(ctx, st.meetingReader(), note, &budget)
	requirements.NoError(err)
	requirements.Len(occurrences, 1)
	assertions.True(occurrences[0].Exact)
	assertions.Equal(cancelled, occurrences[0].Row.MessageID)
	assertions.Equal("cancelled", occurrences[0].Row.Event.Status)
}
