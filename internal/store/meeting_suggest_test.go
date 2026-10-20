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

func TestMeetingLinksSuggestionsRankingAndReadOnly(t *testing.T) {
	assertions := assert.New(t)
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
	var queries atomic.Int64
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
		if strings.Contains(query, "FROM message_recipients mr JOIN participants p") {
			queries.Add(1)
		}
		return nil
	}})
	suggestions, err := observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 3)
	assertions.Equal(int64(1), queries.Load())
	for _, suggestion := range suggestions {
		assertions.Equal(30, suggestion.OverlapMinutes)
		assertions.Equal(1, suggestion.SharedAttendees)
	}
	// Circleback's room URL stays a suggestion, including when the times overlap.
	assertions.Empty(linkedMeetingRefs(t, st, notes[0])[notes[0]])
	reverse, err := observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: notes[0]}, 1)
	requirements.NoError(err)
	requirements.Len(reverse, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), reverse[0].Ref)
	assertions.Equal(int64(2), queries.Load())
	requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(`{"id":"event","status":"cancelled","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), "gcal_json"))
	suggestions, err = observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, 10)
	requirements.NoError(err)
	assertions.Empty(suggestions)
	reverse, err = observed.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: notes[0]}, 10)
	requirements.NoError(err)
	assertions.Empty(reverse)
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
		{name: "scheduled", sent: "2020-01-01T00:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"window@example.org","startsAt":"2027-03-14T10:00:00Z"}}`, include: true},
		{name: "scheduled without UID", sent: "2020-01-01T00:00:00Z", raw: `{"schema_version":1,"recording":{"startedAt":"2020-01-01T00:00:00Z","endedAt":"2020-01-01T00:30:00Z"},"calendar_event":{"startsAt":"2027-03-14T10:00:00Z"}}`, include: true},
		{name: "long recording", sent: "2027-03-12T10:00:00Z", raw: `{"started_at":"2027-03-12T10:00:00Z","ended_at":"2027-03-14T10:30:00Z"}`, include: true},
		{name: "offset", sent: "2027-03-14T06:30:00-04:00", raw: `{}`, include: true},
		{name: "scheduled offset", sent: "2020-01-01T00:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T06:30:00-04:00"}}`, include: true},
		{name: "received", received: "2027-03-14T10:00:00Z", raw: `{}`, include: true},
		{name: "internal", internal: "2027-03-14T10:00:00Z", raw: `{}`, include: true},
		{name: "past", sent: "2020-01-01T00:00:00Z", raw: `{}`},
		{name: "future", sent: "2030-01-01T00:00:00Z", raw: `{}`},
		{name: "scheduled overrides sent", sent: "2027-03-14T10:00:00Z", raw: `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"window@example.org","startsAt":"2030-01-01T00:00:00Z"}}`},
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
	for _, suggestion := range suggestions {
		got = append(got, suggestion.MemberMessageID)
		if suggestion.MemberMessageID == uidless {
			assertions.Equal(fmt.Sprintf("evt:%d", event), suggestion.Ref)
			assertions.Equal(30, suggestion.OverlapMinutes)
		}
	}
	assertions.ElementsMatch(want, got)
	suggestions, err = st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: uidless}, 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(uidless, suggestions[0].MemberMessageID)
	assertions.Equal(fmt.Sprintf("evt:%d", event), suggestions[0].Ref)
	assertions.Equal(30, suggestions[0].OverlapMinutes)
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
					if assert.Len(suggestions, 1) {
						assert.Equal(fmt.Sprintf("evt:%d", event), suggestions[0].Ref)
						assert.Equal(note, suggestions[0].MemberMessageID)
						assert.Equal(30, suggestions[0].OverlapMinutes)
					}
				})
			}
		})
	}
}

func TestMeetingLinksAllDayExceptionUsesSeriesZone(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","iCalUID":"weekly@example.org","start":{"date":"2027-03-07","timeZone":"America/New_York"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, edit := calendarFixture(t, st, "edit", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"weekly","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-15T02:00:00Z"}}`)
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	require.NoError(err)
	require.Len(page.Occurrences, 1)
	listed := page.Occurrences[0]
	assert.Equal("2027-03-14T05:00:00Z", listed.Start.UTC().Format(time.RFC3339))
	assert.Equal("2027-03-15T04:00:00Z", listed.End.UTC().Format(time.RFC3339))
	for _, ref := range []OccurrenceRef{{MessageID: master, Key: "20270314"}, {MessageID: edit}} {
		require.NoError(st.LinkMeetingContext(t.Context(), note, ref))
		links, err := st.MeetingLinksContext(t.Context(), note)
		require.NoError(err)
		require.Len(links, 1)
		assert.True(listed.Start.Equal(links[0].Start), "resolved start must match the calendar listing")
		assert.True(listed.End.Equal(links[0].End), "resolved end must match the calendar listing")
		suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: ref}, 10)
		require.NoError(err)
		if assert.Len(suggestions, 1) {
			assert.Equal(note, suggestions[0].MemberMessageID)
			assert.Equal(listed.Ref, suggestions[0].Ref)
			assert.Equal(30, suggestions[0].OverlapMinutes)
		}
	}
	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note}, 10)
	require.NoError(err)
	if assert.Len(suggestions, 1) {
		assert.Equal(listed.Ref, suggestions[0].Ref)
		assert.Equal(30, suggestions[0].OverlapMinutes)
	}
}

func TestMeetingLinksAllDayExceptionWithUnsupportedRecurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"weekly","iCalUID":"weekly@example.org","start":{"date":"2027-03-07","timeZone":"America/New_York"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`
	_, master := calendarFixture(t, st, "master", raw)
	_, edit := calendarFixture(t, st, "edit", `{"id":"edited","iCalUID":"weekly@example.org","recurringEventId":"weekly","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"startsAt":"2027-03-14T00:00:00Z"}}`)
	ref := OccurrenceRef{MessageID: master, Key: "20270314"}
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, ref))
	requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(strings.Replace(raw, "RRULE:FREQ=WEEKLY;COUNT=3", "RRULE:FREQ=HOURLY", 1)), "gcal_json"))
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	listed := page.Occurrences[0]
	assertions.Equal(fmt.Sprintf("evt:%d@20270314", master), listed.Ref)
	assertions.Equal(edit, listed.MessageID)
	assertions.Equal("2027-03-14T00:00:00Z", listed.Start.UTC().Format(time.RFC3339))
	assertions.Equal("2027-03-15T00:00:00Z", listed.End.UTC().Format(time.RFC3339))
	assertions.Equal([]CalendarUnexpandable{{MessageID: master, Reason: "unsupported recurrence"}}, page.Unexpandable)

	t.Run("saved link", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		links, err := st.MeetingLinksContext(t.Context(), note)
		require.NoError(err)
		require.Len(links, 1)
		assert.Equal(listed.Ref, links[0].Ref)
		assert.True(listed.Start.Equal(links[0].Start))
		assert.True(listed.End.Equal(links[0].End))
	})
	for _, target := range []MeetingLinkRef{{Kind: "evt", OccurrenceRef: ref}, {Kind: "msg", MessageID: note}} {
		t.Run("suggest/"+target.Kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			suggestions, err := st.SuggestMeetingLinksContext(t.Context(), target, 10)
			require.NoError(err)
			require.Len(suggestions, 1)
			assert.Equal(listed.Ref, suggestions[0].Ref)
			assert.Equal(note, suggestions[0].MemberMessageID)
			assert.Equal(30, suggestions[0].OverlapMinutes)
		})
	}
	for _, target := range []OccurrenceRef{ref, {MessageID: edit}} {
		t.Run(fmt.Sprintf("link/%d", target.MessageID), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			require.NoError(st.LinkMeetingContext(t.Context(), note, target))
			links, err := st.MeetingLinksContext(t.Context(), note)
			require.NoError(err)
			require.Len(links, 1)
			assert.Equal(listed.Ref, links[0].Ref)
			assert.True(listed.Start.Equal(links[0].Start))
			assert.True(listed.End.Equal(links[0].End))
		})
	}
}

func TestMeetingSuggestionsUnidentifiedExceptionInheritMasterCancellation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","iCalUID":"weekly@example.org","status":"cancelled","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, exception := calendarFixture(t, st, "exception", `{"id":"unidentified","iCalUID":"weekly@example.org","recurringEventId":"weekly","status":"confirmed","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, note := projectionFixture(t, st, "note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:15:00Z"}}`)

	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	resolved, err := resolveMeetingOccurrence(ctx, st.meetingReader(ctx), OccurrenceRef{MessageID: exception}, &budget)
	requirements.NoError(err)
	assertions.Equal("cancelled", resolved.Row.Event.Status)

	exceptionRef := OccurrenceRef{MessageID: exception}
	seriesRef := OccurrenceRef{MessageID: master, Key: "20270314T100000Z"}
	for _, ref := range []MeetingLinkRef{
		{Kind: "evt", OccurrenceRef: exceptionRef},
		{Kind: "evt", OccurrenceRef: seriesRef},
		{Kind: "msg", MessageID: note},
	} {
		suggestions, err := st.SuggestMeetingLinksContext(t.Context(), ref, 10)
		requirements.NoError(err)
		assertions.Empty(suggestions, "cancelled master occurrences must not be suggested for %s", ref.Kind)
	}
}
