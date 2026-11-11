package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	for _, mode := range []string{"file", "email"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			text := string(calendarDocument("REQUEST", "DTSTAMP:20270101T000000Z\r\nDTSTART:20270115T090000"))
			text = strings.Replace(text, "BEGIN:VEVENT", "X-WR-TIMEZONE:America/New_York\r\nBEGIN:VEVENT", 1)
			text = strings.Replace(text, "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:second@example.com\r\nDTSTAMP:20270101T000000Z\r\nDTSTART:20270115T090000\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
			raw := []byte(text)
			var source int64
			if mode == "file" {
				source = icsSource(t, st)
				_, err := st.PersistICalendarContext(t.Context(), source, raw)
				requirements.NoError(err)
			} else {
				_, source = persistInviteEmail(t, st, "declared-zone", inviteEmail(raw))
			}
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 2)
			for _, id := range ids {
				row := calendarRows(t, st, id)[0]
				requirements.NotNil(row.Event.Start)
				requirements.Equal("2027-01-15T14:00:00Z", row.Event.Start.Format(time.RFC3339))
				requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
					return st.refreshCalendarEventProjectionWith(t.Context(), tx, id)
				}))
				row = calendarRows(t, st, id)[0]
				requirements.Equal("2027-01-15T14:00:00Z", row.Event.Start.Format(time.RFC3339))
			}
		})
	}
}

func TestICalendarRangeKeepsSiblingsAndReportsSeries(t *testing.T) {
	master := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=10\r\nSUMMARY:Series")
	rangeEvent := calendarDocument("REQUEST", "RECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Moved")
	sibling := []byte(strings.Replace(string(calendarDocument("REQUEST", "DTSTART:20270308T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Sibling")), "family@example.com", "sibling@example.com", 1))
	for _, mode := range []string{"file", "email"} {
		for _, reversed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-reversed-%t", mode, reversed), func(t *testing.T) {
				requirements := require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				source := icsSource(t, st)
				if mode == "email" {
					account, err := st.GetOrCreateSource("mbox", "owner@example.com")
					requirements.NoError(err)
					source = account.ID
				}
				items := [][]byte{master, rangeEvent, sibling}
				if reversed {
					items[0], items[1] = items[1], items[0]
				}
				for i, item := range items {
					if mode == "email" {
						persistInviteEmailForSource(t, st, source, fmt.Sprintf("range-%d", i), inviteEmail(item))
					} else {
						_, err := st.PersistICalendarContext(t.Context(), source, item)
						requirements.NoError(err)
					}
				}
				from := time.Date(2027, 3, 8, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				requirements.Len(page.Occurrences, 1)
				requirements.Equal("Sibling", page.Occurrences[0].Subject)
				requirements.Len(page.Unexpandable, 1)
			})
		}
	}
	t.Run("range cancellation", func(t *testing.T) {
		requirements := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		source := icsSource(t, st)
		for _, event := range [][]byte{
			calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=10"),
			calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"),
			calendarDocument("CANCEL", "SEQUENCE:1\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z"),
		} {
			_, err := st.PersistICalendarContext(t.Context(), source, event)
			requirements.NoError(err)
		}
		from := time.Date(2027, 3, 8, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err)
		assert.Empty(t, page.Occurrences)
		requirements.Len(page.Unexpandable, 1)
		_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:2\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
		requirements.NoError(err)
		page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err)
		requirements.Empty(page.Unexpandable)
		requirements.Len(page.Occurrences, 1)
	})
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

func TestICalendarEqualRevisionIgnored(t *testing.T) {
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
}

func TestICalendarNewerStampBumpsRevision(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z")
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	before, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	later := strings.Replace(string(raw), "20270301T120000Z", "20270301T130000Z", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	after, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before)
}

func TestICalendarCancellationWinsRevisionTie(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nSUMMARY:First\r\nRRULE:FREQ=DAILY;COUNT=2\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org")
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	accepted := calendarRows(t, st, ids[0])[0]
	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTAMP:20270301T120000Z")
	before, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	after, err := st.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before)
	row := calendarRows(t, st, ids[0])[0]
	requirements.NotEmpty(accepted.Event.Attendees)
	assertions.Equal(accepted.Event.Attendees, row.Event.Attendees)
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("series", row.Event.Kind)
	assertions.Equal(accepted.Subject, row.Subject)
	assertions.Equal(accepted.Event.Start, row.Event.Start)
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

func TestICalendarCancelledExceptionConversationStats(t *testing.T) {
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

func TestICalendarCancellationInheritsMissingEnd(t *testing.T) {
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
}

func TestICalendarCancellationKeepsImplicitAllDayEnd(t *testing.T) {
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
