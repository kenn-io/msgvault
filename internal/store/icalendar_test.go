package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func calendarDocument(method, props string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:" + method + "\r\nBEGIN:VEVENT\r\nUID:family@example.com\r\n" + props + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
}
func calendarFamilyDocument() []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:family@example.com\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Original series\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:family@example.com\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Moved occurrence\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
}
func icsSource(t *testing.T, st *Store) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("ics", "synthetic-calendar")
	require.NoError(t, err)
	return source.ID
}

func TestICalendarPersistenceRevisionsAndRebuild(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nSUMMARY:First\r\nRRULE:FREQ=DAILY;COUNT=2")
	first, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	assertions.Equal(1, first.Changed)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	id := ids[0]
	duplicate, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	assertions.Equal(1, duplicate.Unchanged)
	assertions.Equal(ids, emailEvents(t, st, source))
	older := strings.Replace(string(raw), "SEQUENCE:2", "SEQUENCE:1", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(older))
	requirements.NoError(err)
	stored, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	later := strings.Replace(string(raw), "20270301T120000Z", "20270301T130000Z", 1)
	later = strings.Replace(later, "20270302T120000Z", "20270302T140000Z", 1)
	later = strings.Replace(later, "20270302T130000Z", "20270302T150000Z", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	assertions.Equal("2027-03-02T14:00:00Z", calendarRows(t, st, id)[0].Event.Start.Format(time.RFC3339))

	stored, err = st.GetMessageRaw(id)
	requirements.NoError(err)
	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTAMP:20270301T130000Z")
	_, err = st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)
	row := calendarRows(t, st, id)[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("series", row.Event.Kind)
	assertions.NotNil(row.Event.Start)
	_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, id)
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, stored, "ics"))
	assertions.Equal(row.Event, calendarRows(t, st, id)[0].Event)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(later))
	requirements.NoError(err)
	assertions.Equal("cancelled", calendarRows(t, st, id)[0].Event.Status)
	restore := strings.Replace(string(raw), "SEQUENCE:2", "SEQUENCE:4", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(restore))
	requirements.NoError(err)
	assertions.Equal("confirmed", calendarRows(t, st, id)[0].Event.Status)
}

func TestICalendarRawIsNotEmailDedupEvidence(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	first := string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	event := "BEGIN:VEVENT\r\nUID:other@example.com\r\nDTSTART:20270303T120000Z\r\nEND:VEVENT\r\n"
	raw := []byte(strings.Replace(first, "END:VCALENDAR\r\n", event+"END:VCALENDAR\r\n", 1))
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	requirements.NoError(err)
	requirements.Len(emailEvents(t, st, source), 2)
	candidates, err := st.GetAllRawMIMECandidates(source)
	requirements.NoError(err)
	assert.Empty(t, candidates, "the shared VCALENDAR bytes do not prove the VEVENT messages are duplicates")
}

func TestICalendarUnchangedUnsupportedSeries(t *testing.T) {
	for _, rule := range []string{"RRULE:FREQ=HOURLY;COUNT=2", "EXRULE:FREQ=DAILY;COUNT=1"} {
		t.Run(rule, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\n"+rule)
			first, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			assertions.Equal(1, first.Changed)
			assertions.Equal(1, first.Unexpandable)
			again, err := st.PersistICalendarContext(t.Context(), source, raw)
			requirements.NoError(err)
			assertions.Zero(again.Changed)
			assertions.Equal(1, again.Unchanged)
			assertions.Equal(1, again.Unexpandable)
		})
	}
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
	t.Run("unedited occurrence inherits master", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		source := icsSource(t, st)
		_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Team meeting\r\nATTENDEE:mailto:guest@example.org\r\nORGANIZER:mailto:owner@example.com"))
		requirements.NoError(err)
		_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "STATUS:CANCELLED\r\nRECURRENCE-ID:20270303T120000Z"))
		requirements.NoError(err)
		from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err)
		requirements.Len(page.Occurrences, 1)
		assertions.Equal("Team meeting", page.Occurrences[0].Subject)
		assertions.Equal("cancelled", page.Occurrences[0].Status)
		assertions.Len(page.Occurrences[0].Attendees, 2)
	})
}
