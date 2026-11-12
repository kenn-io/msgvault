package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func importICalendarForTest(t *testing.T, st *Store, sourceID int64, raw []byte) (ICalendarImportSummary, error) {
	t.Helper()
	prepared, err := PrepareICalendar(raw)
	if err != nil {
		return ICalendarImportSummary{}, err
	}
	return st.PersistICalendarContext(t.Context(), sourceID, prepared)
}

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
	_, err := importICalendarForTest(t, st, source, []byte(text))
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
			_, err := importICalendarForTest(t, st, source, raw)
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
				calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:Original\r\nORGANIZER:mailto:organizer@example.com"),
				calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T140000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:Updated\r\nORGANIZER:mailto:organizer@example.com"),
				calendarDocument("CANCEL", "SEQUENCE:2\r\nORGANIZER:mailto:organizer@example.com"),
			} {
				if mode == "email" {
					persistInviteEmailForSource(t, st, source, fmt.Sprintf("cancelled-series-%d", i), inviteEmail(event))
				} else {
					_, err := importICalendarForTest(t, st, source, event)
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

func TestICalendarRangeMakesSeriesUnexpandable(t *testing.T) {
	for _, mode := range []string{"file", "email"} {
		for _, tc := range []struct{ method, properties string }{
			{"REQUEST", "DTSTART:20270303T140000Z\r\nDURATION:PT1H"},
			{"CANCEL", ""},
		} {
			t.Run(mode+"/"+tc.method, func(t *testing.T) {
				r := require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				var source int64
				if mode == "email" {
					src, err := st.GetOrCreateSource("mbox", "owner@example.com")
					r.NoError(err)
					source = src.ID
				} else {
					source = icsSource(t, st)
				}
				master := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=20\r\nORGANIZER:mailto:organizer@example.com")
				edit := calendarDocument(tc.method, "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\n"+tc.properties+"\r\nORGANIZER:mailto:organizer@example.com")
				if mode == "email" {
					persistInviteEmailForSource(t, st, source, "range-master", inviteEmail(master))
					persistInviteEmailForSource(t, st, source, "range-edit", inviteEmail(edit))
				} else {
					_, err := importICalendarForTest(t, st, source, master)
					r.NoError(err)
					_, err = importICalendarForTest(t, st, source, edit)
					r.NoError(err)
				}
				from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
				for _, day := range []int{0, 8} {
					start := from.AddDate(0, 0, day)
					page, err := st.ListCalendarOccurrencesContext(t.Context(), start, start.AddDate(0, 0, 1))
					r.NoError(err)
					r.Empty(page.Occurrences)
					r.Len(page.Unexpandable, 1)
					r.Equal("unsupported recurrence: RANGE=THISANDFUTURE", page.Unexpandable[0].Reason)
				}
				replacement := calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T150000Z\r\nDURATION:PT1H\r\nORGANIZER:mailto:organizer@example.com")
				if mode == "email" {
					persistInviteEmailForSource(t, st, source, "range-replacement", inviteEmail(replacement))
				} else {
					_, err := importICalendarForTest(t, st, source, replacement)
					r.NoError(err)
				}
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
				r.NoError(err)
				r.Empty(page.Unexpandable)
				r.Len(page.Occurrences, 3)
				r.Equal(15, page.Occurrences[1].Start.Hour())
			})
		}
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
	first, err := importICalendarForTest(t, st, source, raw)
	requirements.NoError(err)
	requirements.Equal(1, first.Changed)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	changed := []byte(strings.Replace(string(raw), "SUMMARY:First", "SUMMARY:Changed", 1))
	duplicate, err := importICalendarForTest(t, st, source, changed)
	requirements.NoError(err)
	assertions.Zero(duplicate.Changed)
	assertions.Equal(1, duplicate.Unchanged)
	assertions.Equal(ids, emailEvents(t, st, source))
	before, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	later := strings.Replace(string(changed), "20270301T120000Z", "20270301T130000Z", 1)
	_, err = importICalendarForTest(t, st, source, []byte(later))
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
	_, err := importICalendarForTest(t, st, source, raw)
	requirements.NoError(err)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTAMP:20270301T120000Z")
	before, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	_, err = importICalendarForTest(t, st, source, cancel)
	requirements.NoError(err)
	after, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before)
	row := calendarRows(t, st, ids[0])[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("series", row.Event.Kind)
	_, err = importICalendarForTest(t, st, source, raw)
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
	_, err := importICalendarForTest(t, st, source, raw)
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
	_, err := importICalendarForTest(t, st, source, calendarFamilyDocument())
	requirements.NoError(err)
	_, err = importICalendarForTest(t, st, source, calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z"))
	requirements.NoError(err)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal("cancelled", page.Occurrences[1].Status)
	assertions.Equal("Original series", page.Occurrences[1].Subject)
	var count int
	var last time.Time
	requirements.NoError(st.db.QueryRow(`SELECT c.message_count,c.last_message_at FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE m.id=?`, emailEvents(t, st, source)[0]).Scan(&count, &last))
	assertions.Equal(2, count)
	assertions.Equal(time.Date(2027, 3, 3, 14, 0, 0, 0, time.UTC), last.UTC())
}

func TestICalendarCancelledExceptionOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name, props, subject string
		emails               []string
	}{
		{"unedited occurrence inherits master", "", "Team meeting", []string{"guest@example.org", "owner@example.com"}},
		{"explicit sparse fields kept", "SUMMARY:Called off\r\nATTENDEE:mailto:other@example.org\r\n", "Team meeting", []string{"guest@example.org", "owner@example.com"}},
		{"start without details inherits master", "DTSTART:20270303T120000Z\r\n", "Team meeting", []string{"guest@example.org", "owner@example.com"}},
		{"start with explicit fields kept", "DTSTART:20270303T120000Z\r\nSUMMARY:Called off\r\nATTENDEE:mailto:other@example.org\r\n", "Team meeting", []string{"guest@example.org", "owner@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, source := persistInviteEmail(t, st, "master", emailFrom("owner@example.com", calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Team meeting\r\nATTENDEE:mailto:guest@example.org\r\nORGANIZER:mailto:owner@example.com")))
			cancel := []byte(strings.ReplaceAll(string(calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\n"+tc.props+"ORGANIZER:mailto:owner@example.com")), "METHOD:CANCEL\r\n", ""))
			mail := []byte(strings.Replace(string(emailFrom("owner@example.com", cancel)), "text/calendar", "text/calendar; method=CANCEL", 1))
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
		{"explicit zero duration", "DURATION:PT0S\r\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			_, err := importICalendarForTest(t, st, source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
			requirements.NoError(err)
			_, err = importICalendarForTest(t, st, source, calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T120000Z\r\n"+tc.duration))
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
		_, err := importICalendarForTest(t, st, source, []byte(raw))
		requirements.NoError(err)
		_, err = importICalendarForTest(t, st, source, calendarDocument("CANCEL", "SEQUENCE:1\r\nSTATUS:CANCELLED"))
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
	summary, err := importICalendarForTest(t, st, source, []byte(raw))
	requirements.NoError(err)
	requirements.Equal(2, summary.Changed)
}

func TestICalendarOversizedRevisionPreservesEvent(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	initial := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Original")
	_, err := importICalendarForTest(t, st, source, initial)
	requirements.NoError(err)
	revision := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Updated\r\nDESCRIPTION:"+strings.Repeat("x", maxMeetingRawBytes))
	_, err = importICalendarForTest(t, st, source, revision)
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
	_, err := importICalendarForTest(t, st, source, raw)
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
	_, err := importICalendarForTest(t, st, source, raw)
	require.ErrorContains(t, err, "Custom/Test")
	var count int
	require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE source_id=?`, source).Scan(&count))
	require.Zero(t, count)
}

func TestICalendarNominalDurationUnexpandable(t *testing.T) {
	for _, tc := range []struct{ name, props string }{
		{"nominal duration", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D"},
		{"spring gap", "DTSTART;TZID=America/New_York:20270314T023000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			_, err := importICalendarForTest(t, st, source, calendarDocument("REQUEST", tc.props+"\r\nRRULE:FREQ=DAILY;COUNT=3"))
			r.NoError(err)
			from := time.Date(2027, 3, 13, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 4))
			r.NoError(err)
			r.Len(page.Unexpandable, 1)
			r.Empty(page.Occurrences)
			r.Contains(page.Unexpandable[0].Reason, "unsupported")
		})
	}
}
