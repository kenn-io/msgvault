package store

import (
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

func TestICalendarPersistenceRevisionsAndRebuild(t *testing.T) {
	for _, behavior := range []string{"duplicate persist", "older sequence", "newer stamp", "cancellation", "rebuild"} {
		t.Run(behavior, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			description := strings.Repeat("Meeting details. ", 20)
			raw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nSUMMARY:First\r\nRRULE:FREQ=DAILY;COUNT=2\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nDESCRIPTION:"+description+"\\nJoin: https://meet.example.com/meeting\r\nLOCATION:Conference room")
			first, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			requirements.Equal(1, first.Changed)
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			id := ids[0]

			switch behavior {
			case "duplicate persist":
				duplicate, err := st.PersistICalendarContext(t.Context(), source, raw)
				requirements.NoError(err)
				assertions.Zero(duplicate.Changed)
				assertions.Equal(1, duplicate.Unchanged)
				assertions.Equal(ids, emailEvents(t, st, source))
				return
			case "older sequence":
				older := strings.Replace(string(raw), "SEQUENCE:2", "SEQUENCE:1", 1)
				_, err := st.PersistICalendarContext(t.Context(), source, []byte(older))
				requirements.NoError(err)
				stored, err := st.GetMessageRaw(id)
				requirements.NoError(err)
				assertions.Equal(raw, stored)
				return
			}

			later := strings.NewReplacer("20270301T120000Z", "20270301T130000Z", "20270302T120000Z", "20270302T140000Z", "20270302T130000Z", "20270302T150000Z").Replace(string(raw))
			before, err := st.DerivedDataRevisionContext(t.Context())
			requirements.NoError(err)
			_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
			requirements.NoError(err)
			if behavior == "newer stamp" {
				after, err := st.DerivedDataRevisionContext(t.Context())
				requirements.NoError(err)
				assertions.Greater(after, before)
				assertions.Equal("2027-03-02T14:00:00Z", calendarRows(t, st, id)[0].Event.Start.Format(time.RFC3339))
				return
			}

			stored, err := st.GetMessageRaw(id)
			requirements.NoError(err)
			accepted := calendarRows(t, st, id)[0]
			cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTAMP:20270301T130000Z")
			before, err = st.DerivedDataRevisionContext(t.Context())
			requirements.NoError(err)
			_, err = st.PersistICalendarContext(t.Context(), source, cancel)
			requirements.NoError(err)
			switch behavior {
			case "cancellation":
				after, err := st.DerivedDataRevisionContext(t.Context())
				requirements.NoError(err)
				assertions.Greater(after, before)
				row := calendarRows(t, st, id)[0]
				requirements.NotEmpty(accepted.Event.Attendees)
				assertions.Equal(accepted.Event.Attendees, row.Event.Attendees)
				assertions.Equal("cancelled", row.Event.Status)
				assertions.Equal("series", row.Event.Kind)
				assertions.Equal(accepted.Subject, row.Subject)
				assertions.Equal(accepted.Event.Start, row.Event.Start)
				_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
				requirements.NoError(err)
				assertions.Equal("cancelled", calendarRows(t, st, id)[0].Event.Status)
			case "rebuild":
				row := calendarRows(t, st, id)[0]
				_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, id)
				requirements.NoError(err)
				requirements.NoError(st.UpsertMessageRawWithFormat(id, stored, "ics"))
				assertions.Equal(row.Event, calendarRows(t, st, id)[0].Event)
			}
		})
	}
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

func TestICalendarCancelledExceptionOccurrences(t *testing.T) {
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
	for _, tc := range []struct {
		name, props, subject string
		emails               []string
	}{
		{"unedited occurrence inherits master", "", "Team meeting", []string{"guest@example.org", "owner@example.com"}},
		{"explicit sparse fields kept", "SUMMARY:Called off\r\nATTENDEE:mailto:other@example.org\r\n", "Called off", []string{"other@example.org", "owner@example.com"}},
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

func TestICalendarCancellationArrivalOrder(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Cancelled meeting")
	_, err := st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Older"))
	requirements.NoError(err)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	row := calendarRows(t, st, ids...)[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("Older", row.Subject)
	requirements.NotNil(row.Event.Start)
	assertions.Equal("2027-03-02T13:00:00Z", row.Event.Start.Format(time.RFC3339))
	stored, err := st.GetMessageRaw(ids[0])
	requirements.NoError(err)
	assertions.Equal(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Older"), stored)
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
