package store

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestCalendarFractionalExceptionPreservesGeneratedOccurrences(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, exception := calendarFixture(t, st, "fractional-edit", `{"id":"fractional-edit","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.123456Z"},"start":{"dateTime":"2027-03-09T19:00:00Z"},"end":{"dateTime":"2027-03-09T20:00:00Z"}}`)
	_, single := calendarFixture(t, st, "unrelated", `{"id":"unrelated","start":{"dateTime":"2027-03-10T12:00:00Z"},"end":{"dateTime":"2027-03-10T13:00:00Z"}}`)

	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 1, 0))
	requirements.NoError(err)
	var ids []int64
	var starts []string
	for _, occurrence := range page.Occurrences {
		ids = append(ids, occurrence.MessageID)
		starts = append(starts, occurrence.Start.UTC().Format(time.RFC3339))
		if occurrence.MessageID == exception {
			assertions.Equal(identityRef(exception), occurrence.Ref)
			assertions.Equal("2027-03-09T20:00:00Z", occurrence.End.UTC().Format(time.RFC3339))
		}
	}
	assertions.Equal([]int64{master, master, exception, single, master}, ids)
	assertions.Equal([]string{"2027-03-02T17:00:00Z", "2027-03-09T17:00:00Z", "2027-03-09T19:00:00Z", "2027-03-10T12:00:00Z", "2027-03-16T17:00:00Z"}, starts)
	assertions.Equal([]CalendarUnexpandable{{MessageID: exception, Reason: "exception original start must have whole-second precision"}}, page.Unexpandable)
}

func TestMeetingFractionalExceptionKeepsMessageIdentity(t *testing.T) {
	for _, archivedMaster := range []bool{false, true} {
		t.Run(strconv.FormatBool(archivedMaster), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var master int64
			if archivedMaster {
				_, master = calendarFixture(t, st, "master", linkSeriesRaw)
			}
			_, exception := calendarFixture(t, st, "fractional", `{"id":"fractional","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.123456Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
			_, note := identityNote(t, st, "note", "")
			// Orphan fractional decisions already retain the original key internally.
			_, err := st.db.Exec(meetingDecisionUpsert, note, "", sql.NullInt64{Int64: exception, Valid: true}, "20270314T100000Z", "link", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
			requirements.NoError(err)
			ref := identityRef(exception)
			from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			requirements.NoError(err)
			defer release()
			budget := calrecur.RequestReadBudget
			resolved, err := resolveMeetingOccurrence(ctx, st.meetingReader(ctx), OccurrenceRef{MessageID: exception}, &budget)
			if archivedMaster {
				requirements.NoError(err)
				masters, err := meetingEventRows(ctx, st.meetingReader(ctx), "e.message_id = ?", []any{master}, &budget)
				requirements.NoError(err)
				requirements.Len(masters, 1)
				expanded, err := expandMeetingSeries(ctx, st.meetingReader(ctx), masters[0], from, from.AddDate(0, 0, 1), &budget)
				requirements.NoError(err)
				requirements.Len(expanded, 2)
				for _, occurrence := range expanded {
					if occurrence.Row.MessageID == exception {
						assertions.Equal(OccurrenceRef{MessageID: exception}, occurrence.Ref)
						assertions.Equal(meetingIdentity{EventMessageID: exception, OccurrenceKey: "20270314T100000Z"}, occurrence.Identity)
					}
				}
			}
			release()
			requirements.NoError(err)
			assertions.Equal(OccurrenceRef{MessageID: exception}, resolved.Ref)
			assertions.Equal(meetingIdentity{EventMessageID: exception, OccurrenceKey: "20270314T100000Z"}, resolved.Identity)
			assertions.Equal(from.Add(12*time.Hour), resolved.Start)
			assertions.Equal(from.Add(13*time.Hour), resolved.End)
			identities, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}, TranscriptIDs: []int64{note}})
			requirements.NoError(err)
			assertions.Equal("occ:"+ref, identities.ByOccurrence[ref].Key)
			assertions.Equal(ref, identities.ByTranscript[note].OccurrenceRef)
			requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
			requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: exception}))
			links, err := st.MeetingLinksContext(t.Context(), note)
			requirements.NoError(err)
			assertions.Empty(links)
		})
	}
}
