package store

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func calendarDocument(method, props string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nMETHOD:" + method + "\r\nBEGIN:VEVENT\r\nUID:family@example.com\r\n" + props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
}
func calendarFamilyDocument() []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:family@example.com\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Original series\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:family@example.com\r\nDTSTAMP:20270301T120000Z\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Moved occurrence\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
}
func icsSource(t *testing.T, st *Store) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("ics", "synthetic-calendar")
	require.NoError(t, err)
	return source.ID
}

func TestICalendarDeclaredTimeZone(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	text := string(calendarDocument("REQUEST", "DTSTAMP:20270101T000000Z\r\nDTSTART:20270115T090000"))
	text = strings.Replace(text, "BEGIN:VEVENT", "X-WR-TIMEZONE:America/New_York\r\nBEGIN:VEVENT", 1)
	text = strings.Replace(text, "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:second@example.com\r\nDTSTAMP:20270101T000000Z\r\nDTSTART:20270115T090000\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, []byte(text))
	requirements.NoError(err)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 2)
	for _, id := range ids {
		row := calendarRows(t, st, id)[0]
		requirements.NotNil(row.Event.Start)
		requirements.Equal("2027-01-15T14:00:00Z", row.Event.Start.Format(time.RFC3339))
	}
}

func TestICalendarInvalidFloatingRecurrenceDate(t *testing.T) {
	for _, name := range []string{"RDATE", "EXDATE"} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			property := name + ":20270303T090000,invalid"
			raw := calendarDocument("REQUEST", "DTSTART:20270302T090000Z\r\nRRULE:FREQ=DAILY;COUNT=2\r\n"+property)
			_, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			requirements.Contains(calendarRows(t, st, ids[0])[0].Event.Recurrence, property)
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			requirements.NoError(err)
			requirements.Empty(page.Occurrences)
			requirements.Len(page.Unexpandable, 1)
			requirements.Equal(ids[0], page.Unexpandable[0].MessageID)
		})
	}
}

func TestICalendarCancelledSeriesOccurrences(t *testing.T) {
	for _, mode := range []string{"file", "email"} {
		t.Run(mode, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			if mode == "email" {
				mail, err := st.GetOrCreateSource("mbox", "owner@example.com")
				r.NoError(err)
				source = mail.ID
			}
			for i, event := range [][]byte{
				calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:Original"),
				calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T140000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:Updated"),
				calendarDocument("CANCEL", "SEQUENCE:2"),
			} {
				if mode == "email" {
					persistInviteEmailForSource(t, st, source, fmt.Sprintf("cancelled-series-%d", i), inviteEmail(event))
				} else {
					_, err := st.PersistICalendarContext(t.Context(), source, event)
					r.NoError(err)
				}
			}
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			r.NoError(err)
			r.Len(page.Occurrences, 3)
			for _, occurrence := range page.Occurrences {
				r.Equal("cancelled", occurrence.Status)
				r.Equal("Updated", occurrence.Subject)
				r.Equal(14, occurrence.Start.Hour())
			}
		})
	}
}

func TestICalendarRangeKeepsSiblings(t *testing.T) {
	master := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=10\r\nSUMMARY:Series")
	rangeEvent := calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Moved")
	sibling := []byte(strings.Replace(string(calendarDocument("REQUEST", "DTSTART:20270308T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Sibling")), "family@example.com", "sibling@example.com", 1))
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprintf("reversed-%t", reversed), func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			items := [][]byte{master, rangeEvent, sibling}
			if reversed {
				items[0], items[1] = items[1], items[0]
			}
			for _, item := range items {
				_, err := st.PersistICalendarContext(t.Context(), source, item)
				requirements.NoError(err)
			}
			before := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			prefix, err := st.ListCalendarOccurrencesContext(t.Context(), before, before.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(prefix.Occurrences, 1)
			requirements.Equal("2027-03-02T12:00:00Z", prefix.Occurrences[0].Start.Format(time.RFC3339))
			requirements.Empty(prefix.Unexpandable)
			from := time.Date(2027, 3, 8, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 2)
			requirements.Equal("2027-03-08T12:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
			requirements.Equal("Sibling", page.Occurrences[0].Subject)
			requirements.Equal("2027-03-08T14:00:00Z", page.Occurrences[1].Start.Format(time.RFC3339))
			requirements.Empty(page.Unexpandable)
		})
	}
}

func TestICalendarRangePreservesOriginalPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, master, boundary string
		want                   []string
		extra                  bool
		windowOffset           int
	}{
		{"timed", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=7", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270305T120000Z\r\nDTSTART:20270305T130000Z", []string{"2027-03-02T12:00:00Z", "2027-03-04T12:00:00Z", "2027-03-04T14:00:00Z", "2027-03-05T13:00:00Z", "2027-03-07T13:00:00Z", "2027-03-08T12:00:00Z", "2027-03-08T13:00:00Z"}, true, 0},
		{"all day", "DTSTART;VALUE=DATE:20270302\r\nRRULE:FREQ=DAILY;COUNT=7", "RECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20270305\r\nDTSTART;VALUE=DATE:20270305\r\nSUMMARY:Updated", []string{"2027-03-02T00:00:00Z", "2027-03-03T00:00:00Z", "2027-03-04T00:00:00Z", "2027-03-05T00:00:00Z", "2027-03-06T00:00:00Z", "2027-03-07T00:00:00Z", "2027-03-08T00:00:00Z"}, false, 0},
		{"single", "DTSTART:20270302T120000Z\r\nDURATION:PT1H", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270305T120000Z\r\nDTSTART:20270305T130000Z", []string{"2027-03-02T12:00:00Z", "2027-03-05T13:00:00Z"}, false, 0},
		{"long occurrence", "DTSTART:20270302T120000Z\r\nDURATION:P7D\r\nRRULE:FREQ=WEEKLY", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T130000Z", []string{"2027-03-02T12:00:00Z"}, false, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			events := [][]byte{calendarDocument("REQUEST", tc.master), calendarDocument("REQUEST", tc.boundary)}
			if tc.extra {
				events = append(events,
					calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270308T120000Z\r\nDURATION:PT1H"),
					calendarDocument("REQUEST", "RECURRENCE-ID:20270306T120000Z\r\nDTSTART:20270304T140000Z\r\nDURATION:PT1H"),
					calendarDocument("CANCEL", "RECURRENCE-ID:20270304T120000Z"))
			}
			for _, event := range events {
				_, err := st.PersistICalendarContext(t.Context(), source, event)
				requirements.NoError(err)
			}
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			to := from.AddDate(0, 0, 7)
			if tc.windowOffset != 0 {
				from = from.AddDate(0, 0, tc.windowOffset)
				to = from.AddDate(0, 0, 1)
			}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, to)
			requirements.NoError(err)
			var starts []string
			for _, occurrence := range page.Occurrences {
				starts = append(starts, occurrence.Start.Format(time.RFC3339))
			}
			requirements.Equal(tc.want, starts)
			requirements.Empty(page.Unexpandable)
			if tc.extra {
				requirements.Equal("cancelled", page.Occurrences[1].Status)
				page, err = st.ListCalendarOccurrencesContext(t.Context(), from.AddDate(0, 0, 2), from.AddDate(0, 0, 3))
				requirements.NoError(err)
				requirements.Len(page.Occurrences, 2)
				requirements.Equal("cancelled", page.Occurrences[0].Status)
				requirements.Equal("2027-03-04T14:00:00Z", page.Occurrences[1].Start.Format(time.RFC3339))
				requirements.Empty(page.Unexpandable)
			}
		})
	}
}

func TestICalendarRangeMovesAroundLaterEdits(t *testing.T) {
	master := "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=10"
	move := "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"
	for _, tc := range []struct {
		name      string
		events    [][]byte
		want      []string
		plainMove bool
	}{
		{"cancelled occurrence", [][]byte{calendarDocument("CANCEL", "RECURRENCE-ID:20270304T120000Z")},
			[]string{"2027-03-03T14:00:00Z", "2027-03-04T14:00:00Z cancelled", "2027-03-05T14:00:00Z"}, false},
		{"cancelled range", [][]byte{calendarDocument("CANCEL", "SEQUENCE:1\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z")},
			[]string{"2027-03-03T14:00:00Z cancelled", "2027-03-04T14:00:00Z cancelled", "2027-03-05T14:00:00Z cancelled"}, false},
		{"cancelled range after plain edit", [][]byte{calendarDocument("CANCEL", "SEQUENCE:1\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z")},
			[]string{"2027-03-03T14:00:00Z cancelled", "2027-03-04T14:00:00Z cancelled", "2027-03-05T14:00:00Z cancelled"}, true},
		{"exception with its own start", [][]byte{calendarDocument("REQUEST", "RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270304T180000Z\r\nDURATION:PT1H")},
			[]string{"2027-03-03T14:00:00Z", "2027-03-04T18:00:00Z", "2027-03-05T14:00:00Z"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			initialMove := move
			if tc.plainMove {
				initialMove = strings.Replace(move, ";RANGE=THISANDFUTURE", "", 1)
			}
			for _, event := range append([][]byte{calendarDocument("REQUEST", master), calendarDocument("REQUEST", initialMove)}, tc.events...) {
				_, err := st.PersistICalendarContext(t.Context(), source, event)
				requirements.NoError(err)
			}
			from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			requirements.NoError(err)
			var got []string
			for _, occurrence := range page.Occurrences {
				line := occurrence.Start.Format(time.RFC3339)
				if occurrence.Status == "cancelled" {
					line += " cancelled"
				}
				got = append(got, line)
			}
			requirements.Equal(tc.want, got)
			requirements.Empty(page.Unexpandable)
			if tc.plainMove {
				from = time.Date(2027, 3, 8, 0, 0, 0, 0, time.UTC)
				page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				requirements.Len(page.Occurrences, 1)
				requirements.Equal("cancelled", page.Occurrences[0].Status)
				requirements.Empty(page.Unexpandable)
				_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:2\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
				requirements.NoError(err)
				page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				requirements.Empty(page.Unexpandable)
				requirements.Len(page.Occurrences, 1)
				requirements.Equal("confirmed", page.Occurrences[0].Status)
			}
		})
	}
}

func TestICalendarRangeLoadsDistantIndividualEdits(t *testing.T) {
	for _, tc := range []struct{ name, method, edit, want, status string }{
		{"cancellation", "CANCEL", "RECURRENCE-ID:20270304T120000Z", "2027-03-11T14:00:00Z", "cancelled"},
		{"moved exception", "REQUEST", "RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270315T180000Z\r\nDURATION:PT1H", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			for _, event := range [][]byte{
				calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=4"),
				calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270310T140000Z\r\nDURATION:PT1H"),
				calendarDocument(tc.method, tc.edit),
			} {
				_, err := st.PersistICalendarContext(t.Context(), source, event)
				r.NoError(err)
			}
			from := time.Date(2027, 3, 11, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			r.NoError(err)
			if tc.want == "" {
				r.Empty(page.Occurrences)
				moved := time.Date(2027, 3, 15, 0, 0, 0, 0, time.UTC)
				page, err = st.ListCalendarOccurrencesContext(t.Context(), moved, moved.AddDate(0, 0, 1))
				r.NoError(err)
				r.Len(page.Occurrences, 1)
				r.Equal("2027-03-15T18:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
			} else {
				r.Len(page.Occurrences, 1)
				r.Equal(tc.want, page.Occurrences[0].Start.Format(time.RFC3339))
				r.Equal("2027-03-11T15:00:00Z", page.Occurrences[0].End.Format(time.RFC3339))
				r.Equal(tc.status, page.Occurrences[0].Status)
			}
			r.Empty(page.Unexpandable)
		})
	}
}

func TestICalendarRangeCancellationInheritsEarlierMove(t *testing.T) {
	for _, tc := range []struct{ name, master, move, cancel, resume, start, end, resumed string }{
		{"timed", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=5", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT2H", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270304T120000Z", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270306T120000Z\r\nDTSTART:20270306T180000Z\r\nDURATION:PT1H", "2027-03-04T14:00:00Z", "2027-03-04T16:00:00Z", "2027-03-06T18:00:00Z"},
		{"all day", "DTSTART;VALUE=DATE:20270302\r\nRRULE:FREQ=DAILY;COUNT=5", "RECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20270303\r\nDTSTART;VALUE=DATE:20270304\r\nDURATION:P2D", "RECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20270304", "RECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20270306\r\nDTSTART;VALUE=DATE:20270308", "2027-03-05T00:00:00Z", "2027-03-07T00:00:00Z", "2027-03-08T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			for _, event := range [][]byte{calendarDocument("REQUEST", tc.master+"\r\nSUMMARY:Old\r\nATTENDEE:mailto:old@example.org"), calendarDocument("REQUEST", tc.move+"\r\nSUMMARY:New\r\nATTENDEE:mailto:new@example.org"), calendarDocument("CANCEL", tc.cancel), calendarDocument("REQUEST", tc.resume)} {
				_, err := st.PersistICalendarContext(t.Context(), source, event)
				r.NoError(err)
			}
			from := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 6))
			r.NoError(err)
			var cancelled, resumed bool
			for _, occurrence := range page.Occurrences {
				if occurrence.Start.Format(time.RFC3339) == tc.start {
					cancelled = true
					r.Equal(tc.end, occurrence.End.Format(time.RFC3339))
					r.Equal("cancelled", occurrence.Status)
					r.Equal("New", occurrence.Subject)
					r.Len(occurrence.Attendees, 1)
					r.Equal("new@example.org", occurrence.Attendees[0].Email)
				}
				if occurrence.Start.Format(time.RFC3339) == tc.resumed {
					resumed = true
					r.Equal("confirmed", occurrence.Status)
				}
			}
			r.True(cancelled)
			r.True(resumed)
			r.Empty(page.Unexpandable)
		})
	}
	t.Run("successive and individual cancellations", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		source := icsSource(t, st)
		for _, event := range [][]byte{
			calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=8\r\nSUMMARY:Old\r\nATTENDEE:mailto:old@example.org\r\nORGANIZER:mailto:old-owner@example.org"),
			calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:New\r\nATTENDEE:mailto:new@example.org\r\nORGANIZER:mailto:new-owner@example.org"),
			calendarDocument("CANCEL", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270304T120000Z"),
			calendarDocument("CANCEL", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270305T120000Z"),
			calendarDocument("CANCEL", "RECURRENCE-ID:20270306T120000Z\r\nSUMMARY:Special\r\nATTENDEE:mailto:special@example.org\r\nORGANIZER:mailto:special-owner@example.org"),
			calendarDocument("CANCEL", "RECURRENCE-ID:20270307T120000Z"),
		} {
			_, err := st.PersistICalendarContext(t.Context(), source, event)
			r.NoError(err)
		}
		from := time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 4))
		r.NoError(err)
		r.Len(page.Occurrences, 4)
		for i, occurrence := range page.Occurrences {
			r.Equal("cancelled", occurrence.Status)
			r.Equal(14, occurrence.Start.Hour())
			var emails []string
			for _, attendee := range occurrence.Attendees {
				emails = append(emails, attendee.Email)
			}
			if i == 1 {
				r.Equal("Special", occurrence.Subject)
				r.ElementsMatch([]string{"special@example.org", "special-owner@example.org"}, emails)
			} else {
				r.Equal("New", occurrence.Subject)
				r.ElementsMatch([]string{"new@example.org", "new-owner@example.org"}, emails)
			}
		}
	})
}

func TestICalendarFutureRangeEditsDoNotExhaustSeriesLimit(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	override := ""
	if st.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	start := time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)
	for offset := 0; offset < 2001; offset += 500 {
		var conversations, messages, events, raws []string
		var conversationArgs, messageArgs, eventArgs, rawArgs []any
		for i := offset; i < min(offset+500, 2001); i++ {
			uid := fmt.Sprintf("future-%d@example.com", i)
			masterID := calevent.ICSMessageID(uid, "")
			conversationID := int64(100000 + i)
			conversations = append(conversations, "(?,?,?,?)")
			conversationArgs = append(conversationArgs, conversationID, source, masterID, "calendar")
			for part, props := range []string{
				"DTSTART:20300301T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2",
				"RECURRENCE-ID;RANGE=THISANDFUTURE:20300301T120000Z\r\nDTSTART:20300301T140000Z\r\nDURATION:PT1H",
			} {
				messageID := int64(100000 + 2*i + part)
				providerID, seriesID, kind, original, eventStart, recurrence, end := masterID, masterID, "series", any(nil), start, `["RRULE:FREQ=DAILY;COUNT=2"]`, any(start.AddDate(0, 0, 1).Add(time.Hour))
				if part == 1 {
					providerID, kind, original, eventStart, recurrence = calevent.ICSMessageID(uid, "20300301T120000Z"), "exception", start, start.Add(2*time.Hour), `["RECURRENCE-ID;RANGE=THISANDFUTURE"]`
					end = nil
				}
				messages = append(messages, "(?,?,?,?,?)")
				messageArgs = append(messageArgs, messageID, source, conversationID, providerID, "calendar_event")
				events = append(events, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
				eventArgs = append(eventArgs, messageID, 1, "fixture", providerID, seriesID, kind, original, eventStart, eventStart.Add(time.Hour), "confirmed", false, "UTC", recurrence, end)
				raw := strings.Replace(string(calendarDocument("REQUEST", props)), "family@example.com", uid, 1)
				var compressed bytes.Buffer
				writer := zlib.NewWriter(&compressed)
				_, err := writer.Write([]byte(raw))
				r.NoError(err)
				r.NoError(writer.Close())
				raws = append(raws, "(?,?,?,?)")
				rawArgs = append(rawArgs, messageID, compressed.Bytes(), "ics", "zlib")
			}
		}
		_, err := st.db.Exec(`INSERT INTO conversations (id,source_id,source_conversation_id,conversation_type)`+override+` VALUES `+strings.Join(conversations, ","), conversationArgs...)
		r.NoError(err)
		_, err = st.db.Exec(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type)`+override+` VALUES `+strings.Join(messages, ","), messageArgs...)
		r.NoError(err)
		_, err = st.db.Exec(`INSERT INTO calendar_events (message_id,projection_version,content_hash,provider_event_id,series_provider_id,kind,original_start_at,start_at,end_at,status,all_day,calendar_time_zone,recurrence_json,recurrence_end_at) VALUES `+strings.Join(events, ","), eventArgs...)
		r.NoError(err)
		_, err = st.db.Exec(`INSERT INTO message_raw (message_id,raw_data,raw_format,compression) VALUES `+strings.Join(raws, ","), rawArgs...)
		r.NoError(err)
	}
	for _, event := range [][]byte{
		calendarDocument("REQUEST", "DTSTART:20300301T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Shifted"),
		calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20300301T120000Z\r\nDTSTART:20270302T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Shifted"),
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, event)
		r.NoError(err)
	}
	_, current := calendarFixture(t, st, "current-early", `{"id":"current-early","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"}}`)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	r.NoError(err)
	r.Len(page.Occurrences, 2)
	r.Equal(current, page.Occurrences[0].MessageID)
	r.Equal("Shifted", page.Occurrences[1].Subject)
	r.Equal("2027-03-02T14:00:00Z", page.Occurrences[1].Start.Format(time.RFC3339))
}

func TestICalendarRangeMovesEndedSeriesIntoWindow(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, event := range [][]byte{
		calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=1"),
		calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270302T120000Z\r\nDTSTART:20270312T140000Z\r\nDURATION:PT1H"),
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, event)
		r.NoError(err)
	}
	from := time.Date(2027, 3, 12, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	r.NoError(err)
	r.Len(page.Occurrences, 1)
	r.Equal("2027-03-12T14:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	r.Empty(page.Unexpandable)
}

func TestICalendarRangeCancelsLaterOccurrences(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, event := range [][]byte{
		calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=4"),
		calendarDocument("CANCEL", "SEQUENCE:1\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z"),
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, event)
		requirements.NoError(err)
	}
	from := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	requirements.Equal("2027-03-04T12:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	requirements.Equal("2027-03-05T12:00:00Z", page.Occurrences[1].Start.Format(time.RFC3339))
	requirements.Equal("cancelled", page.Occurrences[0].Status)
	requirements.Equal("cancelled", page.Occurrences[1].Status)
	requirements.Empty(page.Unexpandable)
}

func TestICalendarAllDayRangeMovesLaterOccurrences(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, event := range [][]byte{
		calendarDocument("REQUEST", "DTSTART;VALUE=DATE:20270302\r\nRRULE:FREQ=DAILY;COUNT=4"),
		calendarDocument("REQUEST", "RECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20270303\r\nDTSTART;VALUE=DATE:20270304"),
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, event)
		requirements.NoError(err)
	}
	from := time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	requirements.Equal("2027-03-05", page.Occurrences[0].StartDate)
	requirements.Equal("2027-03-06", page.Occurrences[1].StartDate)
	requirements.Empty(page.Unexpandable)
}

func TestICalendarSuccessiveRangeMoves(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, event := range [][]byte{
		calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=4"),
		calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"),
		calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270304T120000Z\r\nDTSTART:20270304T160000Z\r\nDURATION:PT1H"),
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, event)
		requirements.NoError(err)
	}
	from := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	requirements.Equal("2027-03-04T16:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	requirements.Equal("2027-03-05T16:00:00Z", page.Occurrences[1].Start.Format(time.RFC3339))
	requirements.Empty(page.Unexpandable)
}

func TestICalendarRangeReportsIncompletePrefix(t *testing.T) {
	for _, tc := range []struct{ name, master, exception, want, occurrence string }{
		{"unidentified", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=7", "RECURRENCE-ID;VALUE=DATE:20270303\r\nDTSTART:20270304T140000Z\r\nDURATION:PT1H", "exception original start cannot be compared", "2027-03-04T12:00:00Z"},
		{"mismatched", "DTSTART;VALUE=DATE:20270302\r\nRRULE:FREQ=DAILY;COUNT=7", "", "unsupported recurrence range", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			for _, event := range [][]byte{
				calendarDocument("REQUEST", tc.master),
				calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270305T120000Z\r\nDTSTART:20270305T130000Z"),
			} {
				_, err := st.PersistICalendarContext(t.Context(), source, event)
				requirements.NoError(err)
			}
			if tc.exception != "" {
				_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", tc.exception))
				requirements.NoError(err)
			}
			from := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			var reasons []string
			for _, item := range page.Unexpandable {
				reasons = append(reasons, item.Reason)
			}
			requirements.Len(reasons, 1)
			requirements.Contains(reasons[0], tc.want)
			if tc.occurrence != "" {
				requirements.Len(page.Occurrences, 1)
				requirements.Equal(tc.occurrence, page.Occurrences[0].Start.Format(time.RFC3339))
			} else {
				requirements.Empty(page.Occurrences)
			}
		})
	}
}

func TestICSFamilyLookupUsesIndexes(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	t.Setenv("MSGVAULT_TEST_DB", "")
	st := newRFC822IDBackfillBackendStore(t)
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+icsFamilyQuery, 0, 1, 1, "family", "family", 1, "family", 1, "family")
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requirements.NoError(rows.Err())
	plan := strings.Join(details, "\n")
	assertions.Contains(plan, "idx_source_merge_archive_only_original")
	assertions.Contains(plan, "idx_calendar_events_series_id")
}

func testICalendarEqualRevisionIgnored(t *testing.T) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nSUMMARY:First")
	first, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Equal(1, first.Changed)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	changed := []byte(strings.Replace(string(raw), "SUMMARY:First", "SUMMARY:Changed", 1))
	duplicate, err := st.PersistICalendarContext(t.Context(), source, changed)
	requirements.NoError(err)
	assertions.Zero(duplicate.Changed)
	assertions.Equal(1, duplicate.Unchanged)
	assertions.Equal(ids, emailEvents(t, st, source))
	before, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	later := strings.Replace(string(changed), "20270301T120000Z", "20270301T130000Z", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	after, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before)
	assertions.Equal("Changed", calendarRows(t, st, ids[0])[0].Subject)
}

func testICalendarCancellationWinsRevisionTie(t *testing.T) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nSUMMARY:First\r\nRRULE:FREQ=DAILY;COUNT=2\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org")
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTAMP:20270301T120000Z")
	before, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	after, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before)
	row := calendarRows(t, st, ids[0])[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("series", row.Event.Kind)
	_, err = st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, ids[0])[0].Event.Status)
}

func TestICalendarRawIsNotEmailDedupEvidence(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	first := string(calendarDocument("REQUEST", "DTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z"))
	event := "BEGIN:VEVENT\r\nUID:other@example.com\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270303T120000Z\r\nEND:VEVENT\r\n"
	raw := []byte(strings.Replace(first, "END:VCALENDAR\r\n", event+"END:VCALENDAR\r\n", 1))
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(emailEvents(t, st, source), 2)
	candidates, err := st.GetAllRawMIMECandidates(t.Context(), source)
	requirements.NoError(err)
	assert.Empty(t, candidates, "calendar event bytes are not email deduplication evidence")
}

func TestICalendarCancelledExceptionOccurrenceAndConversationStats(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarFamilyDocument())
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z"))
	requirements.NoError(err)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal("cancelled", page.Occurrences[1].Status)
	assertions.Equal("Moved occurrence", page.Occurrences[1].Subject)
	var count int
	var last time.Time
	requirements.NoError(st.db.QueryRow(`SELECT c.message_count,c.last_message_at FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE m.id=?`, emailEvents(t, st, source)[0]).Scan(&count, &last))
	assertions.Equal(2, count)
	assertions.Equal(time.Date(2027, 3, 3, 14, 0, 0, 0, time.UTC), last.UTC())
}

func TestICalendarAllDayRangeCancellationMovesLaterOccurrence(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, event := range [][]byte{
		calendarDocument("REQUEST", "DTSTART;VALUE=DATE:20270301\r\nRRULE:FREQ=DAILY;COUNT=31"),
		calendarDocument("CANCEL", "SEQUENCE:1\r\nRECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20270310\r\nDTSTART;VALUE=DATE:20270303"),
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, event)
		requirements.NoError(err)
	}
	from := time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Empty(page.Unexpandable)
	requirements.Len(page.Occurrences, 1)
	requirements.Equal("2027-03-20", page.Occurrences[0].StartDate)
	requirements.Equal("cancelled", page.Occurrences[0].Status)
}

func TestICalendarRangeChangesDetails(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, event := range [][]byte{
		calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:Old\r\nATTENDEE:mailto:old@example.org"),
		calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nSUMMARY:New\r\nATTENDEE:mailto:new@example.org"),
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, event)
		requirements.NoError(err)
	}
	from := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("New", page.Occurrences[0].Subject)
	var emails []string
	for _, attendee := range page.Occurrences[0].Attendees {
		emails = append(emails, attendee.Email)
	}
	assertions.ElementsMatch([]string{"new@example.org"}, emails)
}

func TestICalendarCancelledExceptionOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name, props, subject string
		emails               []string
	}{
		{"unedited occurrence inherits master", "", "Team meeting", []string{"guest@example.org", "owner@example.com"}},
		{"explicit sparse fields kept", "SUMMARY:Called off\r\nATTENDEE:mailto:other@example.org\r\n", "Called off", []string{"other@example.org", "owner@example.com"}},
		{"start without details inherits master", "DTSTART:20270303T120000Z\r\n", "Team meeting", []string{"guest@example.org", "owner@example.com"}},
		{"start with explicit fields kept", "DTSTART:20270303T120000Z\r\nSUMMARY:Called off\r\nATTENDEE:mailto:other@example.org\r\n", "Called off", []string{"other@example.org", "owner@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, source := persistInviteEmail(t, st, "master", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Team meeting\r\nATTENDEE:mailto:guest@example.org\r\nORGANIZER:mailto:owner@example.com")))
			cancel := []byte(strings.ReplaceAll(string(calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\n"+tc.props+"ORGANIZER:mailto:owner@example.com")), "METHOD:CANCEL\r\n", ""))
			mail := []byte(strings.Replace(string(inviteEmail(cancel)), "text/calendar", "text/calendar; method=CANCEL", 1))
			persistInviteEmailForSource(t, st, source, "sparse-cancel", mail)
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 2)
			retained, err := st.GetMessageRaw(ids[1])
			requirements.NoError(err)
			assertions.Equal(cancel, retained)
			from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(tc.subject, page.Occurrences[0].Subject)
			assertions.Equal("cancelled", page.Occurrences[0].Status)
			var emails []string
			for _, attendee := range page.Occurrences[0].Attendees {
				emails = append(emails, attendee.Email)
			}
			assertions.ElementsMatch(tc.emails, emails)
		})
	}
}

func TestICalendarCancellationEnd(t *testing.T) {
	for _, tc := range []struct {
		name, duration string
		count          int
	}{
		{"missing end", "", 1},
		{"explicit zero duration", "DURATION:PT0S\r\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
			requirements.NoError(err)
			_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T120000Z\r\n"+tc.duration))
			requirements.NoError(err)
			from := time.Date(2027, 3, 3, 12, 30, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.Add(15*time.Minute))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, tc.count)
			if tc.count > 0 {
				assert.Equal(t, "cancelled", page.Occurrences[0].Status)
			}
		})
	}
	t.Run("implicit all-day end", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		source := icsSource(t, st)
		raw := strings.Replace(string(calendarDocument("REQUEST", "DTSTART;VALUE=DATE:20270310")), "METHOD:REQUEST\r\n", "", 1)
		_, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
		requirements.NoError(err)
		_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:1\r\nSTATUS:CANCELLED"))
		requirements.NoError(err)
		from := time.Date(2027, 3, 10, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err)
		requirements.Len(page.Occurrences, 1)
		assertions.Equal("cancelled", page.Occurrences[0].Status)
		assertions.Equal("2027-03-11", page.Occurrences[0].EndDate)
	})
}

func TestICalendarImportsLargeFileWithSmallEvents(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	padding := strings.Repeat("a", maxMeetingRawBytes/2)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nBEGIN:VEVENT\r\nUID:first@example.com\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nX-PADDING:" + padding + "\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:second@example.com\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T130000Z\r\nX-PADDING:" + padding + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	summary, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
	requirements.NoError(err)
	requirements.Equal(2, summary.Changed)
}

func TestICalendarOversizedRevisionPreservesEvent(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	initial := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Original")
	_, err := st.PersistICalendarContext(t.Context(), source, initial)
	requirements.NoError(err)
	revision := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Updated\r\nDESCRIPTION:"+strings.Repeat("x", maxMeetingRawBytes))
	_, err = st.PersistICalendarContext(t.Context(), source, revision)
	requirements.ErrorIs(err, errMeetingRawTooLarge)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("Original", page.Occurrences[0].Subject)
	assertions.Empty(page.Unexpandable)
}

func TestICalendarRoomAttendeesAreNotParticipants(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE:mailto:guest@example.org\r\nATTENDEE;CUTYPE=ROOM:mailto:conference@example.org")
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	for _, tc := range []struct {
		email string
		want  int
	}{
		{"organizer@example.com", 1}, {"guest@example.org", 1},
		{"conference@example.org", 0},
	} {
		var count int
		requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address=?`, tc.email).Scan(&count))
		assertions.Equal(tc.want, count, tc.email)
	}
}

func TestICalendarRejectsInvalidZoneAtomically(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nBEGIN:VTIMEZONE\r\nTZID:Custom/Test\r\nBEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:sibling@example.com\r\nDTSTAMP:20260701T120000Z\r\nDTSTART:20270706T090000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:old@example.com\r\nDTSTAMP:20260701T120000Z\r\nDTSTART;TZID=Custom/Test:20260706T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	require.ErrorContains(t, err, "Custom/Test")
	var count int
	require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE source_id=?`, source).Scan(&count))
	require.Zero(t, count)
}
