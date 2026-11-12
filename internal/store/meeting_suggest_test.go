package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestMeetingSuggestionsRankingSignals(t *testing.T) {
	r := require.New(t)
	start := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	member := suggestMember{ID: 1, Title: "Weekly planning", Start: start, End: start.Add(30 * time.Minute), Emails: map[string]bool{"guest@example.com": true, "room@example.com": true}}
	event := CalendarOccurrence{Ref: "evt:2", Subject: "Weekly planning", Start: start, End: start.Add(30 * time.Minute), Attendees: []calevent.Attendee{{Email: "Guest@example.com"}, {Email: "guest@example.com"}, {Email: "room@example.com", Role: "room"}}}
	result, ok := rankMeetingSuggestion(member, event)
	r.True(ok)
	r.Equal(1, result.SharedAttendees)
	r.Equal(2, result.TitleTokens)
	r.Equal(30, result.OverlapMinutes)
	event.Start = member.End
	event.End = event.Start.Add(time.Hour)
	_, ok = rankMeetingSuggestion(member, event)
	r.False(ok)
}

func TestMeetingSuggestionsUnmatchedNote(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "suggest-event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"a@example.com"},{"email":"b@example.com"}]}`)
	_, other := calendarFixture(t, st, "suggest-other", `{"id":"other","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	data, note := projectionFixture(t, st, "suggest-note", "granola_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	var participants []int64
	for _, email := range []string{"a@example.com", "b@example.com"} {
		participant, err := st.EnsureParticipant(email, "", "")
		r.NoError(err)
		participants = append(participants, participant)
	}
	r.NoError(st.ReplaceMessageRecipients(note, "to", participants, nil))
	ref := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 2)
	r.Equal(fmt.Sprintf("evt:%d", event), page.Suggestions[0].Ref)
	r.Equal(2, page.Suggestions[0].SharedAttendees)
	r.Equal(fmt.Sprintf("evt:%d", other), page.Suggestions[1].Ref)
	eventRef, err := ParseMeetingLinkRef(page.Suggestions[0].Ref)
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref, eventRef, "link")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref)
	r.NoError(err)
	r.Len(record.Links, 1)
	page, err = st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
	r.NoError(err)
	for _, item := range page.Suggestions {
		r.NotEqual(eventRef.MessageID, item.TitleMessageID)
	}
}

func TestMeetingSuggestionsWithheldTitle(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "title-event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	data, note := projectionFixture(t, st, "title-note", "meeting_json", `{}`)
	data.Message.Subject = sql.NullString{String: "Example event", Valid: true}
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	ref := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	for _, id := range []int64{0, note, event} {
		var classify func(context.Context, []int64) (map[int64]bool, error)
		if id != 0 {
			classify = func(_ context.Context, ids []int64) (map[int64]bool, error) {
				r.Contains(ids, id)
				return map[int64]bool{id: true}, nil
			}
		}
		page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, classify)
		r.NoError(err)
		r.Len(page.Suggestions, 1)
		if id == 0 {
			r.Equal(2, page.Suggestions[0].TitleTokens)
		} else {
			r.Zero(page.Suggestions[0].TitleTokens)
		}
	}
}

func TestMeetingSuggestionsRankingAndSavedChoices(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "rank-first", `{"id":"first","iCalUID":"first@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T10:30:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	calendarFixture(t, st, "rank-first-copy", `{"id":"first","iCalUID":"first@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T10:30:00Z"},"attendees":[{"email":"guest@example.com"}]}`, "copy@example.com")
	_, second := calendarFixture(t, st, "rank-second", `{"id":"second","start":{"dateTime":"2027-03-14T10:25:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	data, note := projectionFixture(t, st, "rank-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	participant, err := st.EnsureParticipant("guest@example.com", "", "")
	r.NoError(err)
	r.NoError(st.ReplaceMessageRecipients(note, "to", []int64{participant}, nil))
	noteRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	page, err := st.SuggestMeetingLinksContext(t.Context(), noteRef, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 2)
	r.Equal(fmt.Sprintf("evt:%d", first), page.Suggestions[0].Ref)
	r.Equal(30, page.Suggestions[0].OverlapMinutes)
	r.Equal(fmt.Sprintf("evt:%d", second), page.Suggestions[1].Ref)
	r.Equal(5, page.Suggestions[1].OverlapMinutes)
	firstRef, err := ParseMeetingLinkRef(page.Suggestions[0].Ref)
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "unlink")
	r.NoError(err)
	page, err = st.SuggestMeetingLinksContext(t.Context(), noteRef, 1, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(fmt.Sprintf("evt:%d", second), page.Suggestions[0].Ref)
}

func TestMeetingSuggestionsOccurrences(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "suggest-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	calendarFixture(t, st, "suggest-moved", `{"id":"moved","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"start":{"dateTime":"2027-03-21T13:00:00Z"},"end":{"dateTime":"2027-03-21T14:00:00Z"}}`)
	data, note := projectionFixture(t, st, "suggest-moved-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 21, 13, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	ref, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270321T100000Z", master))
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(note, page.Suggestions[0].MemberMessageID)
}

func TestMeetingSuggestionsFutureSeries(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "near-event", `{"id":"near","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	for i := range 2001 {
		calendarFixture(t, st, fmt.Sprintf("future-series-%d", i), fmt.Sprintf(`{"id":"future-%d","start":{"dateTime":"2027-03-15T23:00:00Z"},"end":{"dateTime":"2027-03-16T00:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY"]}`, i))
	}
	data, note := projectionFixture(t, st, "near-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(fmt.Sprintf("evt:%d", event), page.Suggestions[0].Ref)
}

func TestMeetingSuggestionsOldNotes(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "old-event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	for i := range 2003 {
		data, id := projectionFixture(t, st, fmt.Sprintf("old-note-%d", i), "meeting_json", `{}`)
		at := time.Date(2027, 3, 8, 10, 0, 0, 0, time.UTC)
		if i == 2002 {
			at = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
		}
		data.Message.SentAt = sql.NullTime{Time: at, Valid: true}
		_, err := st.UpsertMessage(data.Message)
		r.NoError(err)
		if i == 0 {
			_, err = st.db.ExecContext(t.Context(), `UPDATE meeting_details SET duration_seconds=? WHERE message_id=?`, 7*24*60*60, id)
			r.NoError(err)
		}
	}
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 2)
}

func TestMeetingSuggestionsSecondLinkedOccurrence(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "linked-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, bridge := projectionFixture(t, st, "linked-bridge", "meeting_json", `{}`)
	data, candidate := projectionFixture(t, st, "linked-candidate", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 21, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	first, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", master))
	r.NoError(err)
	second, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270321T100000Z", master))
	r.NoError(err)
	bridgeRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: bridge}}
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, first, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, second, "link")
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), second, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(candidate, page.Suggestions[0].MemberMessageID)
}

func TestMeetingSuggestionsSavable(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		calendarFixture(t, st, fmt.Sprintf("crowded-%d", i), `{"id":"crowded","iCalUID":"crowded@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, fmt.Sprintf("owner-%d@example.com", i))
	}
	_, alternative := calendarFixture(t, st, "alternative", `{"id":"alternative","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	data, note := projectionFixture(t, st, "candidate-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	ref := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 2)
	r.Equal(fmt.Sprintf("evt:%d", alternative), page.Suggestions[1].Ref)
	for _, suggestion := range page.Suggestions {
		partner, err := ParseMeetingLinkRef(suggestion.Ref)
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref, partner, "link")
		r.NoError(err)
	}
}

func TestMeetingSuggestionsLongNoteAndCancelledEvent(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "long-event", `{"id":"long-event","start":{"dateTime":"2027-03-15T11:00:00Z"},"end":{"dateTime":"2027-03-15T12:00:00Z"}}`)
	_, cancelled := calendarFixture(t, st, "cancelled-event", `{"id":"cancelled","status":"cancelled","start":{"dateTime":"2027-03-15T11:00:00Z"},"end":{"dateTime":"2027-03-15T12:00:00Z"}}`)
	data, note := projectionFixture(t, st, "long-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE meeting_details SET duration_seconds=? WHERE message_id=?`, 26*60*60, note)
	r.NoError(err)
	eventRef := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}
	page, err := st.SuggestMeetingLinksContext(t.Context(), eventRef, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(note, page.Suggestions[0].MemberMessageID)
	cancelledRef := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: cancelled}}
	page, err = st.SuggestMeetingLinksContext(t.Context(), cancelledRef, 10, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
}

func TestMeetingSuggestionsBroadWindow(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendar, event := calendarFixture(t, st, "broad-event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	for i := range 2001 {
		data, _ := projectionFixture(t, st, fmt.Sprintf("broad-note-%d", i), "meeting_json", `{}`)
		data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
		_, err := st.UpsertMessage(data.Message)
		r.NoError(err)
	}
	_, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}, 10, nil)
	r.ErrorIs(err, ErrQueryTooBroad)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{calendar.Message.SourceID})
	page, err := st.SuggestMeetingLinksContext(ctx, MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}, 10, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
}

func TestMeetingSuggestionsAllDayAndPartialExpansion(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "tokyo-day", `{"id":"tokyo","start":{"date":"2027-03-15","timeZone":"Asia/Tokyo"},"end":{"date":"2027-03-16","timeZone":"Asia/Tokyo"}}`)
	_, unsupported := calendarFixture(t, st, "unsupported", `{"id":"unsupported","start":{"dateTime":"2027-03-14T16:00:00Z"},"end":{"dateTime":"2027-03-14T17:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
	_, exception := calendarFixture(t, st, "unsupported-exception", `{"id":"unsupported-edit","recurringEventId":"unsupported","originalStartTime":{"dateTime":"2027-03-14T16:00:00Z"},"start":{"dateTime":"2027-03-14T16:00:00Z"},"end":{"dateTime":"2027-03-14T17:00:00Z"}}`)
	noteData, note := projectionFixture(t, st, "tokyo-note", "meeting_json", `{}`)
	noteData.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 16, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(noteData.Message)
	r.NoError(err)
	occurrences, err := st.ListCalendarOccurrencesContext(t.Context(), noteData.Message.SentAt.Time.Add(-24*time.Hour), noteData.Message.SentAt.Time.Add(25*time.Hour))
	r.NoError(err)
	r.NotEmpty(occurrences.Occurrences)
	r.True(occurrences.Occurrences[0].Start.Before(noteData.Message.SentAt.Time), "%+v", occurrences.Occurrences)
	r.True(occurrences.Occurrences[0].End.After(noteData.Message.SentAt.Time), "%+v", occurrences.Occurrences)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}, 10, nil)
	r.NoError(err)
	r.NotEmpty(page.Unexpandable)
	r.Equal(unsupported, page.Unexpandable[0].MessageID)
	reverse, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}, 10, nil)
	r.NoError(err)
	r.Len(reverse.Suggestions, 1)
	r.Equal(note, reverse.Suggestions[0].MemberMessageID)
	_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}, MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}, "link")
	r.NoError(err)
	r.Len(page.Suggestions, 2)
	r.Equal(fmt.Sprintf("evt:%d", event), page.Suggestions[0].Ref)
	r.Equal(fmt.Sprintf("evt:%d", exception), page.Suggestions[1].Ref)
}

func TestMeetingSuggestionsExcludeConflictingLink(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "conflict-first", `{"id":"first","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, second := calendarFixture(t, st, "conflict-second", `{"id":"second","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, bridge := projectionFixture(t, st, "conflict-bridge", "meeting_json", `{}`)
	data, note := projectionFixture(t, st, "conflict-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	a := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: first}}
	b := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: second}}
	bridgeRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: bridge}}
	noteRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, a, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, b, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, a, "unlink")
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), noteRef, 10, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, b, "link")
	r.ErrorIs(err, ErrMeetingLinkConflict)
}

func TestMeetingSuggestionsLimit(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 12 {
		calendarFixture(t, st, fmt.Sprintf("limit-event-%d", i), fmt.Sprintf(`{"id":"event-%d","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, i))
	}
	data, note := projectionFixture(t, st, "limit-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	ref := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	for _, tc := range []struct{ limit, want int }{{0, 10}, {1, 1}, {101, 12}} {
		page, err := st.SuggestMeetingLinksContext(t.Context(), ref, tc.limit, nil)
		r.NoError(err)
		r.Len(page.Suggestions, tc.want)
		for i := 1; i < len(page.Suggestions); i++ {
			r.Less(page.Suggestions[i-1].Ref, page.Suggestions[i].Ref)
		}
	}
}
