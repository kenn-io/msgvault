package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingGranolaAllDaySuggestionUsesScheduledDuration(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", `{"id":"event","start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	data, note := projectionFixture(t, st, "note", "granola_json", `{"calendar_event":{"scheduled_start_time":"2027-03-14","scheduled_end_time":"2027-03-15"}}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	require.NoError(err)
	page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.NoError(err)
	require.Len(page.Suggestions, 1)
	assert.Equal(fmt.Sprintf("evt:%d", event), page.Suggestions[0].Ref)
	assert.Equal(60, page.Suggestions[0].OverlapMinutes)
}

func TestMeetingSuggestionsDeduplicateCopiesBeforeLimit(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarFixture(t, st, "first", `{"id":"first","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T09:30:00Z"},"end":{"dateTime":"2027-03-14T10:15:00Z"}}`)
	_, best := calendarFixture(t, st, "best", singleLinkRaw("best", "shared@example.org"), "copy@example.com")
	_, other := calendarFixture(t, st, "other", `{"id":"other","iCalUID":"other@example.org","start":{"dateTime":"2027-03-14T10:20:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	data, note := projectionFixture(t, st, "note", "circleback_json", `{"meeting":{"url":"https://meet.example.com/room"}}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.UpsertMessage(data.Message)
	requirements.NoError(err)
	for _, limit := range []int{10, 2} {
		page, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, limit)
		requirements.NoError(err)
		requirements.Len(page.Suggestions, 2)
		assertions.Equal(fmt.Sprintf("evt:%d", best), page.Suggestions[0].Ref)
		assertions.Equal(fmt.Sprintf("evt:%d", other), page.Suggestions[1].Ref)
	}
}

func meetingSuggestionBatchFixture(t *testing.T) (*Store, int64, []int64) {
	t.Helper()
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	participant, err := st.EnsureParticipant("guest@example.com", "Guest Example", "")
	requirements.NoError(err)
	var notes []int64
	for i := range 3 {
		data, id := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "circleback_json", `{"meeting":{"url":"https://meet.example.com/room"}}`)
		data.Message.SentAt.Time = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
		data.Message.SentAt.Valid = true
		_, err := st.UpsertMessage(data.Message)
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{participant}, nil))
		notes = append(notes, id)
	}
	return st, event, notes
}

func TestMeetingLinksSuggestionsBatchesParticipantsAndWritesNothing(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, event, notes := meetingSuggestionBatchFixture(t)
	var queries atomic.Int64
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
		if strings.Contains(query, "FROM message_recipients mr JOIN participants p") {
			queries.Add(1)
		}
		return nil
	}})
	suggestions, err := observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions.Suggestions, 3)
	assertions.Equal(int64(1), queries.Load())
	for _, suggestion := range suggestions.Suggestions {
		assertions.Equal(30, suggestion.OverlapMinutes)
		assertions.Equal(1, suggestion.SharedAttendees)
	}
	// Circleback's room URL stays a suggestion, including when the times overlap.
	assertions.Empty(linkedMeetingRefs(t, st, notes[0])[notes[0]])
	reverse, err := observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: notes[0]}, 1)
	requirements.NoError(err)
	requirements.Len(reverse.Suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), reverse.Suggestions[0].Ref)
	assertions.Equal(int64(2), queries.Load())
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingLinksSuggestionsTimeWindow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "window@example.org"))
	var want []int64
	var uidless int64
	for _, tc := range []struct {
		name, sent, received, internal, raw string
		include                             bool
	}{
		{name: "scheduled and linked", sent: "2020-01-01T00:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"window@example.org","startsAt":"2027-03-14T10:00:00Z"}}`},
		{name: "scheduled without UID", sent: "2020-01-01T00:00:00Z", raw: `{"schema_version":1,"recording":{"startedAt":"2020-01-01T00:00:00Z","endedAt":"2020-01-01T00:30:00Z"},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z"}}`, include: true},
		{name: "long recording", sent: "2027-03-12T10:00:00Z", raw: `{"started_at":"2027-03-12T10:00:00Z","ended_at":"2027-03-14T10:30:00Z"}`, include: true},
		{name: "offset", sent: "2027-03-14T06:30:00-04:00", raw: `{}`, include: true},
		{name: "scheduled offset", sent: "2020-01-01T00:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T06:30:00-04:00"}}`, include: true},
		{name: "received", received: "2027-03-14T10:00:00Z", raw: `{}`, include: true},
		{name: "internal", internal: "2027-03-14T10:00:00Z", raw: `{}`, include: true},
		{name: "past", sent: "2020-01-01T00:00:00Z", raw: `{}`},
		{name: "future", sent: "2030-01-01T00:00:00Z", raw: `{}`},
		{name: "scheduled overrides sent", sent: "2027-03-14T10:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2030-01-01T00:00:00Z"}}`},
		{name: "ends at start", sent: "2027-03-14T09:30:00Z", raw: `{}`},
		{name: "starts at end", sent: "2027-03-14T11:00:00Z", raw: `{}`},
	} {
		format := "meeting_json"
		if strings.Contains(tc.raw, "schema_version") {
			format = "twenty_json"
		}
		data, id := projectionFixture(t, st, tc.name, format, tc.raw)
		for field, value := range map[*sql.NullTime]string{&data.Message.SentAt: tc.sent, &data.Message.ReceivedAt: tc.received, &data.Message.InternalDate: tc.internal} {
			if value != "" {
				at, err := time.Parse(time.RFC3339, value)
				requirements.NoError(err)
				*field = sql.NullTime{Time: at, Valid: true}
			}
		}
		_, err := st.UpsertMessage(data.Message)
		requirements.NoError(err)
		if tc.name == "scheduled without UID" {
			uidless = id
		}
		if tc.include {
			want = append(want, id)
		}
	}
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 100)
	requirements.NoError(err)
	var got []int64
	for _, suggestion := range suggestions.Suggestions {
		got = append(got, suggestion.MemberMessageID)
		if suggestion.MemberMessageID == uidless {
			assertions.Equal(fmt.Sprintf("evt:%d", event), suggestion.Ref)
			assertions.Equal(30, suggestion.OverlapMinutes)
		}
	}
	assertions.ElementsMatch(want, got)
	suggestions, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: uidless}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions.Suggestions, 1)
	assertions.Equal(uidless, suggestions.Suggestions[0].MemberMessageID)
	assertions.Equal(fmt.Sprintf("evt:%d", event), suggestions.Suggestions[0].Ref)
	assertions.Equal(30, suggestions.Suggestions[0].OverlapMinutes)
	assertions.Empty(linkedMeetingRefs(t, st, uidless)[uidless])
}

func TestMeetingLinksSuggestionsAllDayAcrossUTCDates(t *testing.T) {
	for _, tc := range []struct{ zone, noteStart string }{
		{"Asia/Tokyo", "2027-03-01T18:00:00Z"},
		{"America/Los_Angeles", "2027-03-03T06:00:00Z"},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", fmt.Sprintf(`{"id":"event","start":{"date":"2027-03-02","timeZone":%q},"end":{"date":"2027-03-03","timeZone":%q}}`, tc.zone, tc.zone))
			_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"startsAt":%q}}`, tc.noteStart))
			for _, ref := range []MeetingLinkRef{{Kind: "evt", MessageID: event}, {Kind: "msg", MessageID: note}} {
				t.Run(ref.Kind, func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					suggestions, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10)
					require.NoError(err)
					if assert.Len(suggestions.Suggestions, 1) {
						assert.Equal(fmt.Sprintf("evt:%d", event), suggestions.Suggestions[0].Ref)
						assert.Equal(note, suggestions.Suggestions[0].MemberMessageID)
						assert.Equal(30, suggestions.Suggestions[0].OverlapMinutes)
					}
				})
			}
		})
	}
}

func TestMeetingLinksAllDayExceptionMasterStates(t *testing.T) {
	const normal = `{"id":"weekly","iCalUID":"weekly@example.org","start":{"date":"2027-03-07","timeZone":"America/New_York"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
	const timed = `{"id":"weekly","iCalUID":"weekly@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
	for _, tc := range []struct {
		name, initial, changed, original, key, reason, noteStart string
		cancelled                                                bool
	}{
		{"normal", normal, normal, `{"date":"2027-03-14"}`, "20270314", "", "2027-03-14T12:00:00Z", false},
		{"over budget", normal, `{"id":"weekly","iCalUID":"weekly@example.org","start":{"date":"1800-01-01","timeZone":"America/New_York"},"end":{"date":"1800-01-02"},"recurrence":["RRULE:FREQ=DAILY"]}`, `{"date":"2027-03-14"}`, "20270314", calrecur.ErrBudgetExceeded.Error(), "2027-03-14T12:00:00Z", false},
		{"unsupported", normal, strings.Replace(normal, "RRULE:FREQ=WEEKLY;COUNT=3", "RRULE:FREQ=HOURLY", 1), `{"date":"2027-03-14"}`, "20270314", "unsupported recurrence", "2027-03-14T00:00:00Z", false},
		{"cancelled", timed, `{"id":"weekly","status":"cancelled"}`, `{"dateTime":"2027-03-14T10:00:00Z"}`, "20270314T100000Z", "", "2027-03-14T12:00:00Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", tc.initial)
			_, edit := calendarFixture(t, st, "edit", fmt.Sprintf(`{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"weekly","originalStartTime":%s,"start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`, tc.original))
			_, note := projectionFixture(t, st, "note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"startsAt":%q}}`, tc.noteStart))
			ref := OccurrenceRef{MessageID: master, Key: tc.key}
			require.NoError(st.UpsertMessageRawWithFormat(master, []byte(tc.changed), "gcal_json"))
			from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			require.NoError(err)
			require.Len(page.Occurrences, 1)
			listed := page.Occurrences[0]
			assert.Equal(formatMeetingOccurrence(ref), listed.Ref)
			assert.Equal(edit, listed.MessageID)
			assert.Equal(from, listed.Start.UTC())
			assert.Equal(from.AddDate(0, 0, 1), listed.End.UTC())
			if tc.reason != "" {
				assert.Equal([]CalendarUnexpandable{{MessageID: master, Reason: tc.reason}}, page.Unexpandable)
			}
			if tc.cancelled {
				assert.Equal("cancelled", listed.Status)
			}
			for _, target := range []MeetingLinkRef{{Kind: "evt", OccurrenceRef: ref}, {Kind: "evt", MessageID: edit}, {Kind: "msg", MessageID: note}} {
				suggestions, err := st.SuggestMeetingLinksContext(t.Context(), target, 10)
				require.NoError(err)
				if tc.cancelled {
					assert.Empty(suggestions.Suggestions)
					continue
				}
				require.Len(suggestions.Suggestions, 1)
				assert.Equal(listed.Ref, suggestions.Suggestions[0].Ref)
				assert.Equal(note, suggestions.Suggestions[0].MemberMessageID)
				assert.Equal(30, suggestions.Suggestions[0].OverlapMinutes)
			}
			require.NoError(st.UpsertMessageRawWithFormat(master, []byte(tc.initial), "gcal_json"))
			require.NoError(st.LinkMeetingContext(t.Context(), note, ref))
			require.NoError(st.UpsertMessageRawWithFormat(master, []byte(tc.changed), "gcal_json"))
			links, err := meetingLinksForTest(t, st, note)
			require.NoError(err)
			require.Len(links, 1)
			assert.Equal(listed.Ref, links[0].Ref)
			assert.True(listed.Start.Equal(links[0].Start))
			assert.True(listed.End.Equal(links[0].End))
			for _, target := range []OccurrenceRef{ref, {MessageID: edit}} {
				require.NoError(st.LinkMeetingContext(t.Context(), note, target))
				links, err := meetingLinksForTest(t, st, note)
				require.NoError(err)
				require.Len(links, 1)
				assert.Equal(listed.Ref, links[0].Ref)
				assert.True(listed.Start.Equal(links[0].Start))
				assert.True(listed.End.Equal(links[0].End))
				require.NoError(st.UnlinkMeetingContext(t.Context(), note, target))
				links, err = meetingLinksForTest(t, st, note)
				require.NoError(err)
				assert.Empty(links)
			}
		})
	}
}
