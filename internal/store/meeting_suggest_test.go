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
	event.Start = member.End.Add(-20 * time.Second)
	event.End = member.End.Add(time.Minute)
	result, ok = rankMeetingSuggestion(member, event)
	r.True(ok)
	r.Equal(1, result.OverlapMinutes)
	event.Start = member.End
	event.End = event.Start.Add(time.Hour)
	_, ok = rankMeetingSuggestion(member, event)
	r.False(ok)
}

func TestMeetingSuggestionsEventWithManyOtherSeries(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "seed-event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	noteData, note := projectionFixture(t, st, "matching-note", "meeting_json", `{}`)
	noteData.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(noteData.Message)
	r.NoError(err)
	participant, err := st.EnsureParticipant("guest@example.com", "", "")
	r.NoError(err)
	r.NoError(st.ReplaceMessageRecipients(note, "to", []int64{participant}, nil))
	ref := MeetingLinkRef{Kind: "evt", MessageID: event}
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", 2001)
	_, err = st.db.ExecContext(t.Context(), `UPDATE calendar_events SET start_at='2027-03-01 00:00:00+00:00', end_at='2027-03-02 00:00:00+00:00', start_date='2027-03-01', end_date='2027-03-02', recurrence_json='["RRULE:FREQ=DAILY"]' WHERE kind='series'`)
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(note, page.Suggestions[0].MemberMessageID)
	r.Equal(1, page.Suggestions[0].SharedAttendees)
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
	ref := MeetingLinkRef{Kind: "msg", MessageID: note}
	for _, id := range []int64{-1, 0, note, event} {
		seen := map[int64]bool{}
		var classify func(context.Context, []int64) (map[int64]bool, error)
		if id == -1 {
			classify = func(context.Context, []int64) (map[int64]bool, error) { return map[int64]bool{}, nil }
		} else if id != 0 {
			classify = func(_ context.Context, ids []int64) (map[int64]bool, error) {
				r.Contains(ids, id)
				for _, classified := range ids {
					r.False(seen[classified])
					seen[classified] = true
				}
				return map[int64]bool{id: true}, nil
			}
		}
		page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, classify)
		r.NoError(err)
		r.Len(page.Suggestions, 1)
		if id <= 0 {
			r.Equal(2, page.Suggestions[0].TitleTokens)
		} else {
			r.Zero(page.Suggestions[0].TitleTokens)
		}
	}
}

func TestMeetingSuggestionsBackfilledScheduledStart(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "backfill-first", `{"id":"first","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, next := calendarFixture(t, st, "backfill-next", `{"id":"next","start":{"dateTime":"2027-03-14T11:00:00Z"},"end":{"dateTime":"2027-03-14T12:00:00Z"}}`)
	data, note := projectionFixture(t, st, "backfill-note", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2027-03-15T10:00:00Z","summary":{"markdown":"Summary"}},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z","endsAt":"2027-03-14T11:00:00Z"}}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE meeting_details SET projection_version=1, scheduled_start=NULL, scheduled_duration_seconds=NULL WHERE message_id=?`, note)
	r.NoError(err)
	r.NoError(st.backfillMeetingProjectionsContext(t.Context()))
	for _, ref := range []MeetingLinkRef{{Kind: "msg", MessageID: note}, {Kind: "evt", MessageID: first}} {
		page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
		r.NoError(err)
		r.Len(page.Suggestions, 1)
		r.Equal(60, page.Suggestions[0].OverlapMinutes)
		if ref.Kind == "msg" {
			r.Equal(fmt.Sprintf("evt:%d", first), page.Suggestions[0].Ref)
		} else {
			r.Equal(note, page.Suggestions[0].MemberMessageID)
		}
	}
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: next}, 10, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
}

func TestMeetingSuggestionsScheduledIntervals(t *testing.T) {
	for _, tc := range []struct {
		name, format, raw, end, nextEnd                     string
		day, minute, firstOverlap, nextOverlap, startMinute int
	}{
		{"short", "granola_json", `{"calendar_event":{"scheduled_start_time":"2027-03-14T10:00:00Z","scheduled_end_time":"2027-03-14T10:15:00Z"}}`, "10:15", "10:45", 14, 0, 15, 0, 0},
		{"long", "granola_json", `{"calendar_event":{"scheduled_start_time":"2027-03-14T10:00:00Z","scheduled_end_time":"2027-03-14T11:30:00Z"}}`, "11:00", "11:30", 14, 0, 60, 30, 0},
		{"short recording", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2027-03-14T10:00:00Z","endedAt":"2027-03-14T10:20:00Z","summary":{"markdown":"Summary"}},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z","endsAt":"2027-03-14T11:30:00Z"}}`, "11:00", "11:30", 14, 0, 60, 30, 0},
		{"recording without scheduled end", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2027-03-14T10:50:00Z","endedAt":"2027-03-14T11:10:00Z","summary":{"markdown":"Summary"}},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z"}}`, "11:00", "11:30", 14, 50, 10, 10, 50},
		{"circleback recording", "circleback_json", `{"meeting":{"startTime":"2027-03-14T10:00:00Z","endTime":"2027-03-14T11:30:00Z","durationSeconds":1200}}`, "11:00", "11:30", 14, 0, 60, 30, 0},
		{"notion recording", "notion_meeting_json", `{"schema_version":1,"discovery":{"meeting_notes":{"recording":{"start_time":"2027-03-14T10:00:00Z","end_time":"2027-03-14T10:20:00Z"},"calendar_event":{"start_time":"2027-03-14T10:00:00Z","end_time":"2027-03-14T11:30:00Z"}}},"canonical":{"summary":"Summary"}}`, "11:00", "11:30", 14, 0, 60, 30, 0},
		{"late recording", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2027-03-14T10:50:00Z","summary":{"markdown":"Summary"}},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z","endsAt":"2027-03-14T11:00:00Z"}}`, "11:00", "11:30", 14, 50, 60, 0, 0},
		{"twenty recording", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2027-03-15T10:00:00Z","endedAt":"2027-03-15T10:30:00Z"},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z","endsAt":"2027-03-14T10:30:00Z"}}`, "10:30", "11:00", 15, 0, 30, 0, 0},
		{"twenty start only", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z"}}`, "10:30", "11:00", 15, 0, 30, 0, 0},
		{"notion outside window", "notion_meeting_json", `{"schema_version":1,"discovery":{"meeting_notes":{"calendar_event":{"start_time":"2027-03-15T10:00:00Z","end_time":"2027-03-15T11:00:00Z"}}},"canonical":{"summary":"Summary"}}`, "11:00", "11:30", 14, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, first := calendarFixture(t, st, "first-event", fmt.Sprintf(`{"id":"first","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T%s:00Z"}}`, tc.end))
			_, next := calendarFixture(t, st, "next-event", fmt.Sprintf(`{"id":"next","start":{"dateTime":"2027-03-14T%s:00Z"},"end":{"dateTime":"2027-03-14T%s:00Z"}}`, tc.end, tc.nextEnd))
			data, note := projectionFixture(t, st, "interval-note", tc.format, tc.raw)
			data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, tc.day, 10, tc.minute, 0, 0, time.UTC), Valid: true}
			_, err := st.UpsertMessage(data.Message)
			r.NoError(err)
			page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10, nil)
			r.NoError(err)
			count := 0
			if tc.firstOverlap > 0 {
				count++
			}
			if tc.nextOverlap > 0 {
				count++
			}
			r.Len(page.Suggestions, count)
			if tc.firstOverlap > 0 {
				r.Equal(fmt.Sprintf("evt:%d", first), page.Suggestions[0].Ref)
				r.Equal(tc.firstOverlap, page.Suggestions[0].OverlapMinutes)
			}
			if tc.nextOverlap > 0 {
				r.Equal(fmt.Sprintf("evt:%d", next), page.Suggestions[1].Ref)
				r.Equal(tc.nextOverlap, page.Suggestions[1].OverlapMinutes)
			}
			page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: first}, 10, nil)
			r.NoError(err)
			if tc.firstOverlap > 0 {
				r.Len(page.Suggestions, 1)
				r.Equal(tc.firstOverlap, page.Suggestions[0].OverlapMinutes)
				r.Equal(time.Date(2027, 3, 14, 10, tc.startMinute, 0, 0, time.UTC), page.Suggestions[0].Start.UTC())
			} else {
				r.Empty(page.Suggestions)
			}
			page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: next}, 10, nil)
			r.NoError(err)
			if tc.nextOverlap > 0 {
				r.Len(page.Suggestions, 1)
				r.Equal(tc.nextOverlap, page.Suggestions[0].OverlapMinutes)
			} else {
				r.Empty(page.Suggestions)
			}
		})
	}
}

func TestMeetingSuggestionsRescheduled(t *testing.T) {
	for _, tc := range []struct {
		name, format, initial, updated, movedStart, movedEnd string
		oldOverlap, movedOverlap                             int
	}{
		{"end with recording", "twenty_json", `{"schema_version":1,"recording":{"startedAt":"2027-03-14T10:00:00Z","endedAt":"2027-03-14T10:20:00Z"},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z","endsAt":"2027-03-14T10:30:00Z"}}`, `{"schema_version":1,"recording":{"startedAt":"2027-03-14T10:00:00Z","endedAt":"2027-03-14T10:20:00Z"},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z","endsAt":"2027-03-14T11:30:00Z"}}`, "11:00", "11:30", 60, 30},
		{"granola start", "granola_json", `{"summary_text":"Summary","calendar_event":{"scheduled_start_time":"2027-03-14T10:00:00Z","scheduled_end_time":"2027-03-14T11:00:00Z"}}`, `{"summary_text":"Summary","calendar_event":{"scheduled_start_time":"2027-03-14T12:00:00Z","scheduled_end_time":"2027-03-14T13:00:00Z"}}`, "12:00", "13:00", 0, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, first := calendarFixture(t, st, "reschedule-first", `{"id":"first","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
			_, moved := calendarFixture(t, st, "reschedule-moved", fmt.Sprintf(`{"id":"moved","start":{"dateTime":"2027-03-14T%s:00Z"},"end":{"dateTime":"2027-03-14T%s:00Z"}}`, tc.movedStart, tc.movedEnd))
			data, note := projectionFixture(t, st, "reschedule-note", tc.format, tc.initial)
			data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
			_, err := st.UpsertMessage(data.Message)
			r.NoError(err)
			ref := MeetingLinkRef{Kind: "evt", MessageID: moved}
			page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
			r.NoError(err)
			r.Empty(page.Suggestions)
			data.RawMIME = []byte(tc.updated)
			_, err = st.PersistMessage(data)
			r.NoError(err)
			page, err = st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
			r.NoError(err)
			r.Len(page.Suggestions, 1)
			r.Equal(note, page.Suggestions[0].MemberMessageID)
			r.Equal(tc.movedOverlap, page.Suggestions[0].OverlapMinutes)
			page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10, nil)
			r.NoError(err)
			if tc.oldOverlap == 0 {
				r.Len(page.Suggestions, 1)
				r.Equal(fmt.Sprintf("evt:%d", moved), page.Suggestions[0].Ref)
			} else {
				r.Len(page.Suggestions, 2)
			}
			page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: first}, 10, nil)
			r.NoError(err)
			if tc.oldOverlap == 0 {
				r.Empty(page.Suggestions)
			} else {
				r.Len(page.Suggestions, 1)
				r.Equal(tc.oldOverlap, page.Suggestions[0].OverlapMinutes)
			}
		})
	}
}

func TestMeetingSuggestionsScheduledDuration(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "evidence-first", `{"id":"first","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, next := calendarFixture(t, st, "evidence-next", `{"id":"next","start":{"dateTime":"2027-03-14T11:00:00Z"},"end":{"dateTime":"2027-03-14T12:00:00Z"}}`)
	data, note := projectionFixture(t, st, "evidence-note", "circleback_json", `{"meeting":{"startTime":"tomorrow-ish","date":"2027-03-14T10:00:00Z","endTime":"2027-03-14T11:00:00Z"}}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(fmt.Sprintf("evt:%d", first), page.Suggestions[0].Ref)
	r.Equal(60, page.Suggestions[0].OverlapMinutes)
	page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: first}, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), page.Suggestions[0].Start.UTC())
	page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: next}, 10, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
}

func TestMeetingSuggestionsRankingAndSavedChoices(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "rank-first", `{"id":"first","iCalUID":"first@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T10:30:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	calendarFixture(t, st, "rank-first-copy", `{"id":"first","iCalUID":"first@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T10:30:00Z"},"attendees":[{"email":"guest@example.com"}]}`, "copy@example.com")
	_, second := calendarFixture(t, st, "rank-second", `{"id":"second","start":{"dateTime":"2027-03-14T10:25:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com"},{"email":"other@example.com"}]}`)
	data, note := projectionFixture(t, st, "rank-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	participant, err := st.EnsureParticipant("guest@example.com", "", "")
	r.NoError(err)
	other, err := st.EnsureParticipant("other@example.com", "", "")
	r.NoError(err)
	r.NoError(st.ReplaceMessageRecipients(note, "to", []int64{participant, other}, nil))
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	page, err := st.SuggestMeetingLinksContext(t.Context(), noteRef, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 2)
	r.Equal(fmt.Sprintf("evt:%d", second), page.Suggestions[0].Ref)
	r.Equal(2, page.Suggestions[0].SharedAttendees)
	r.Equal(5, page.Suggestions[0].OverlapMinutes)
	r.Equal(fmt.Sprintf("evt:%d", first), page.Suggestions[1].Ref)
	r.Equal(1, page.Suggestions[1].SharedAttendees)
	r.Equal(30, page.Suggestions[1].OverlapMinutes)
	for _, suggestion := range page.Suggestions {
		partner, err := ParseMeetingLinkRef(suggestion.Ref)
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, partner, "link")
		r.NoError(err)
	}
	firstRef, err := ParseMeetingLinkRef(page.Suggestions[0].Ref)
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "unlink")
	r.NoError(err)
	page, err = st.SuggestMeetingLinksContext(t.Context(), noteRef, 1, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
}

func TestMeetingSuggestionsOccurrences(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "suggest-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"],"attendees":[{"email":"guest@example.com"}]}`)
	calendarFixture(t, st, "suggest-moved", `{"id":"moved","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"start":{"dateTime":"2027-03-21T13:00:00Z"},"end":{"dateTime":"2027-03-21T14:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	data, note := projectionFixture(t, st, "suggest-moved-note", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 21, 13, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	r.NoError(err)
	participant, err := st.EnsureParticipant("guest@example.com", "", "")
	r.NoError(err)
	r.NoError(st.ReplaceMessageRecipients(note, "to", []int64{participant}, nil))
	ref, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270321T100000Z", master))
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(note, page.Suggestions[0].MemberMessageID)
	r.Equal(1, page.Suggestions[0].SharedAttendees)
}

func TestMeetingSuggestionsOldNotes(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "old-event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	for i := range 3 {
		format, raw := "meeting_json", `{}`
		if i == 0 {
			format, raw = "circleback_json", `{"meeting":{"durationSeconds":604800}}`
		}
		data, _ := projectionFixture(t, st, fmt.Sprintf("old-note-%d", i), format, raw)
		at := time.Date(2027, 3, 8, 10, 0, 0, 0, time.UTC)
		if i == 2 {
			at = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
		}
		data.Message.SentAt = sql.NullTime{Time: at, Valid: true}
		_, err := st.UpsertMessage(data.Message)
		r.NoError(err)
	}
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 2)
	_, cancelled := calendarFixture(t, st, "cancelled-event", `{"id":"cancelled","status":"cancelled","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: cancelled}, 10, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
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
	bridgeRef := MeetingLinkRef{Kind: "msg", MessageID: bridge}
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, first, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, second, "link")
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), second, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(candidate, page.Suggestions[0].MemberMessageID)
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
	_, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 10, nil)
	r.ErrorIs(err, ErrQueryTooBroad)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{calendar.Message.SourceID})
	page, err := st.SuggestMeetingLinksContext(ctx, MeetingLinkRef{Kind: "evt", MessageID: event}, 10, nil)
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
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10, nil)
	r.NoError(err)
	r.NotEmpty(page.Unexpandable)
	r.Equal(unsupported, page.Unexpandable[0].MessageID)
	reverse, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 10, nil)
	r.NoError(err)
	r.Len(reverse.Suggestions, 1)
	r.Equal(note, reverse.Suggestions[0].MemberMessageID)
	ref, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T160000Z", unsupported))
	r.NoError(err)
	reverse, err = st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
	r.NoError(err)
	r.Len(reverse.Suggestions, 1)
	r.Equal(note, reverse.Suggestions[0].MemberMessageID)
	r.NotEmpty(reverse.Unexpandable)
	_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, MeetingLinkRef{Kind: "evt", MessageID: event}, "link")
	r.NoError(err)
	page, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10, nil)
	r.NoError(err)
	r.NotEmpty(page.Unexpandable)
	r.Equal(unsupported, page.Unexpandable[0].MessageID)
	r.Len(page.Suggestions, 1)
	r.Equal(fmt.Sprintf("evt:%d", exception), page.Suggestions[0].Ref)
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
	a := MeetingLinkRef{Kind: "evt", MessageID: first}
	b := MeetingLinkRef{Kind: "evt", MessageID: second}
	bridgeRef := MeetingLinkRef{Kind: "msg", MessageID: bridge}
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, a, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), bridgeRef, b, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, a, "unlink")
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), noteRef, 10, nil)
	r.NoError(err)
	r.Empty(page.Suggestions)
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
	ref := MeetingLinkRef{Kind: "msg", MessageID: note}
	for _, tc := range []struct{ limit, want int }{{0, 10}, {1, 1}} {
		page, err := st.SuggestMeetingLinksContext(t.Context(), ref, tc.limit, nil)
		r.NoError(err)
		r.Len(page.Suggestions, tc.want)
		for i := 1; i < len(page.Suggestions); i++ {
			previous, err := ParseMeetingLinkRef(page.Suggestions[i-1].Ref)
			r.NoError(err)
			current, err := ParseMeetingLinkRef(page.Suggestions[i].Ref)
			r.NoError(err)
			r.Less(previous.MessageID, current.MessageID)
		}
	}
}

func TestMeetingSuggestionsDeduplicatedSeed(t *testing.T) {
	for _, kind := range []string{"msg", "evt", "recurring"} {
		t.Run(kind, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			recurring := kind == "recurring"
			raw := `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			if recurring {
				raw = `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`
			}
			_, event := calendarFixture(t, st, "seed-event", raw)
			data, note := projectionFixture(t, st, "seed-note", "meeting_json", `{}`)
			data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
			_, err := st.UpsertMessage(data.Message)
			r.NoError(err)
			var survivor, duplicate int64
			if kind == "msg" {
				survivor = note
				_, duplicate = projectionFixture(t, st, "duplicate-note", "meeting_json", `{}`)
			} else {
				survivor = event
				_, duplicate = calendarFixture(t, st, "duplicate-event", raw)
			}
			_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "seed-duplicate")
			r.NoError(err)
			if recurring {
				kind = "evt"
			}
			old := MeetingLinkRef{Kind: kind, MessageID: duplicate}
			current := MeetingLinkRef{Kind: kind, MessageID: survivor}
			if recurring {
				old, err = ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", duplicate))
				r.NoError(err)
				current, err = ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", survivor))
				r.NoError(err)
			}
			want, err := st.SuggestMeetingLinksContext(t.Context(), current, 10, nil)
			r.NoError(err)
			r.Len(want.Suggestions, 1)
			got, err := st.SuggestMeetingLinksContext(t.Context(), old, 10, nil)
			r.NoError(err)
			r.Equal(want, got)
			ctx := sourcescope.WithSourceIDs(t.Context(), []int64{})
			_, err = st.SuggestMeetingLinksContext(ctx, old, 10, nil)
			r.ErrorIs(err, sql.ErrNoRows)
		})
	}
}

func TestMeetingSuggestionsKeepsEligibleGroupedNote(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "grouped-event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"group_20270314T100000Z"}}`
	var notes []int64
	for i, title := range []string{"Other", "Example event"} {
		data, note := projectionFixture(t, st, fmt.Sprintf("grouped-note-%d", i), "granola_json", raw)
		data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
		data.Message.Subject = sql.NullString{String: title, Valid: true}
		_, err := st.UpsertMessage(data.Message)
		r.NoError(err)
		notes = append(notes, note)
	}
	eventRef := MeetingLinkRef{Kind: "evt", MessageID: event}
	_, err := st.DecideMeetingLinkContext(t.Context(), eventRef, MeetingLinkRef{Kind: "msg", MessageID: notes[1]}, "unlink")
	r.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), eventRef, 10, nil)
	r.NoError(err)
	r.Len(page.Suggestions, 1)
	r.Equal(notes[0], page.Suggestions[0].MemberMessageID)
	_, err = st.DecideMeetingLinkContext(t.Context(), eventRef, MeetingLinkRef{Kind: "msg", MessageID: notes[0]}, "link")
	r.NoError(err)
}

func TestMeetingSuggestionsIncompleteEventWarnings(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"missing end", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"}}`},
		{"cancelled", `{"id":"event","status":"cancelled"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "incomplete-event", tc.raw)
			ref := MeetingLinkRef{Kind: "evt", MessageID: event}
			record, err := st.MeetingRecordContext(t.Context(), ref)
			r.NoError(err)
			page, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10, nil)
			r.NoError(err)
			r.Empty(page.Suggestions)
			r.Equal(record.Unexpandable, page.Unexpandable)
		})
	}
}
