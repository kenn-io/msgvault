package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestICalendarEqualSequenceCancellationRecoversBounds(t *testing.T) {
	for _, stamp := range []string{"20270301T110000Z", "20270301T120000Z"} {
		t.Run(stamp, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nSTATUS:CANCELLED")
			first, err := st.PersistICalendarContext(t.Context(), source, cancel)
			requirements.NoError(err)
			_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTAMP:"+stamp+"\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Recovered"))
			requirements.NoError(err)
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal("cancelled", page.Occurrences[0].Status)
			assertions.Equal("Recovered", page.Occurrences[0].Subject)
			metadata, err := st.GetMessageMetadata(first.MessageIDs[0])
			requirements.NoError(err)
			meta, err := calevent.ParseICSMetadata([]byte(metadata.String))
			requirements.NoError(err)
			assertions.Equal(int64(2), meta.Sequence)
			assertions.Equal("2027-03-01T12:00:00Z", meta.Stamp.Format(time.RFC3339))
		})
	}
}

func TestICalendarActiveReplaySkipsUnreadableCancellationEvidence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	cancelled, err := st.PersistICalendarContext(t.Context(), source,
		calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED"))
	requirements.NoError(err)
	requirements.Len(cancelled.MessageIDs, 1)
	_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression=NULL WHERE message_id=?`, []byte("broken calendar"), cancelled.MessageIDs[0])
	requirements.NoError(err)

	active := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z")
	result, err := st.PersistICalendarContext(t.Context(), source, active)
	requirements.NoError(err)
	assertions.Equal(1, result.Unchanged)
	assertions.Equal(1, result.Unexpandable)
	assertions.Contains(strings.Join(result.Diagnostics, "\n"), "decode iCalendar")
	assertions.Equal(cancelled.MessageIDs, result.MessageIDs)
	assertions.Equal("cancelled", calendarRows(t, st, cancelled.MessageIDs...)[0].Event.Status)
}

func TestICalendarCancellationRetainsMovedException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED"))
	requirements.NoError(err)
	snapshot := strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nSEQUENCE:1\r\n")
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(snapshot))
	requirements.NoError(err)
	from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("2027-03-03T14:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
	// A later detached replay must use the retained master's sequence too.
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
	requirements.NoError(err)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("2027-03-03T14:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
}

func TestRandomMessageIDsExcludeEmailCalendarEvents(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "sample", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
	require.Len(t, emailEvents(t, st, source), 1)
	ids, err := st.GetRandomMessageIDs(source, 100)
	require.NoError(t, err)
	assert.Equal(t, []int64{parent}, ids)
}

func TestEmailCalendarBudgetsSpanParts(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		shared, events, parts int
	}{
		{"bytes", 128 << 10, 128, 3},
		{"components", 0, 5001, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var raw strings.Builder
			raw.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=parts\r\n\r\n")
			for range tc.parts {
				raw.WriteString("--parts\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\n")
				for range tc.shared / 1024 {
					raw.WriteString("X-SHARED:" + strings.Repeat("x", 1024) + "\r\n")
				}
				for range tc.events {
					raw.WriteString("BEGIN:VEVENT\r\nUID:budget@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nEND:VEVENT\r\n")
				}
				raw.WriteString("END:VCALENDAR\r\n")
			}
			raw.WriteString("--parts--\r\n")
			parent, source := persistInviteEmail(t, st, "budget-"+tc.name, []byte(raw.String()))
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			assertions.Contains(metadata.String, "limit")
			assertions.Len(emailEvents(t, st, source), 1)
			stored, err := st.GetMessageRaw(parent)
			requirements.NoError(err)
			assertions.Equal(raw.String(), string(stored))
		})
	}
}

func TestICalendarBoundedCancellationFillsSparseTombstone(t *testing.T) {
	for _, recurrence := range []string{"", "RRULE:FREQ=DAILY;COUNT=2\r\n", "RECURRENCE-ID:20270302T120000Z\r\n"} {
		t.Run(recurrence, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", recurrence+"SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nSTATUS:CANCELLED"))
			requirements.NoError(err)
			_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", recurrence+"SEQUENCE:3\r\nDTSTAMP:20270301T130000Z\r\nSTATUS:CANCELLED\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nSUMMARY:Cancelled planning"))
			requirements.NoError(err)
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal("cancelled", page.Occurrences[0].Status)
			assertions.Equal("Cancelled planning", page.Occurrences[0].Subject)
		})
	}
}

func TestEmailCalendarBoundedCancellationFillsSparseTombstone(t *testing.T) {
	for _, recurrence := range []string{"", "RECURRENCE-ID:20270302T120000Z\r\n"} {
		t.Run(recurrence, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			sparse := calendarDocument("CANCEL", recurrence+"SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nORGANIZER:mailto:owner@example.com")
			_, source := persistInviteEmail(t, st, "sparse", inviteEmail(sparse))
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			for _, revision := range []struct{ name, props string }{
				{"older sequence", "SEQUENCE:1\r\nDTSTAMP:20270301T130000Z\r\nORGANIZER:mailto:owner@example.com"},
				{"older stamp", "SEQUENCE:2\r\nDTSTAMP:20270301T110000Z\r\nORGANIZER:mailto:owner@example.com"},
				{"equal revision", "SEQUENCE:2\r\nDTSTAMP:20270301T120000Z\r\nORGANIZER:mailto:owner@example.com"},
				{"other organizer", "SEQUENCE:3\r\nORGANIZER:mailto:other@example.com"},
				{"missing organizer", "SEQUENCE:3"},
			} {
				bounded := calendarDocument("CANCEL", recurrence+revision.props+"\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H")
				persistInviteEmail(t, st, revision.name, inviteEmail(bounded))
				retained, err := st.GetMessageRaw(ids[0])
				requirements.NoError(err)
				assertions.Equal(sparse, retained, revision.name)
			}
			bounded := calendarDocument("CANCEL", recurrence+"SEQUENCE:2\r\nDTSTAMP:20270301T130000Z\r\nORGANIZER:mailto:owner@example.com\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Cancelled planning")
			persistInviteEmail(t, st, "newer stamp", inviteEmail(bounded))
			row := calendarRows(t, st, ids...)[0]
			assertions.Equal("cancelled", row.Event.Status)
			assertions.Equal("Cancelled planning", row.Subject)
			requirements.NotNil(row.Event.Start)
			assertions.Equal("2027-03-02T12:00:00Z", row.Event.Start.Format(time.RFC3339))
		})
	}
}

func TestICalendarBoundedCancellationKeepsAcceptedSnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTAMP:20270301T100000Z\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Accepted"))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTAMP:20270301T130000Z\r\nSTATUS:CANCELLED\r\nDTSTART:20270303T120000Z\r\nDTEND:20270303T130000Z\r\nSUMMARY:Other"))
	requirements.NoError(err)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
	assertions.Equal("Accepted", page.Occurrences[0].Subject)
	assertions.Equal(time.Date(2027, 3, 2, 12, 0, 0, 0, time.UTC), page.Occurrences[0].Start.UTC())
}

func TestICalendarBoundedCancellationKeepsSupersededSnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	accepted := calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Accepted exception")
	first, err := st.PersistICalendarContext(t.Context(), source, accepted)
	requirements.NoError(err)
	for _, raw := range [][]byte{
		calendarDocument("CANCEL", "SEQUENCE:2\r\nRECURRENCE-ID:20270303T120000Z"),
		calendarDocument("REQUEST", "SEQUENCE:3\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"),
		calendarDocument("CANCEL", "SEQUENCE:4\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T160000Z\r\nDURATION:PT1H\r\nSUMMARY:Other"),
	} {
		_, err = st.PersistICalendarContext(t.Context(), source, raw)
		requirements.NoError(err)
	}
	retained, err := st.GetMessageRaw(first.MessageIDs[0])
	requirements.NoError(err)
	assertions.Equal(accepted, retained)
	from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
	assertions.Equal("Accepted exception", page.Occurrences[0].Subject)
	assertions.Equal("2027-03-03T14:00:00Z", page.Occurrences[0].Start.UTC().Format(time.RFC3339))
}

func attendeeEmails(attendees []calevent.Attendee) []string {
	var emails []string
	for _, a := range attendees {
		emails = append(emails, a.Email)
	}
	return emails
}

func TestCalendarNewerSparseCancellationReplacesTombstoneInvitees(t *testing.T) {
	const series = "DTSTAMP:20270301T090000Z\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Series\r\nORGANIZER:mailto:owner@example.com\r\nATTENDEE:mailto:guest@example.com"
	cancel := func(recurrence string, sequence int, invitee string) []byte {
		props := recurrence + fmt.Sprintf("SEQUENCE:%d\r\nDTSTAMP:2027030%dT120000Z\r\nSTATUS:CANCELLED\r\nORGANIZER:mailto:owner@example.com", sequence, sequence)
		if invitee != "" {
			props += "\r\nATTENDEE:mailto:" + invitee
		}
		return calendarDocument("CANCEL", props)
	}
	for _, tc := range []struct {
		name, recurrence string
		email            bool
	}{
		{name: "ics exception", recurrence: "RECURRENCE-ID:20270303T120000Z\r\n"},
		{name: "email exception", recurrence: "RECURRENCE-ID:20270303T120000Z\r\n", email: true},
		{name: "ics standalone"},
		{name: "email standalone", email: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var source int64
			persist := func(key string, raw []byte) {
				t.Helper()
				if tc.email {
					_, source = persistInviteEmail(t, st, key, inviteEmail(raw))
					return
				}
				source = icsSource(t, st)
				_, err := st.PersistICalendarContext(t.Context(), source, raw)
				requirements.NoError(err)
			}
			events := func() []int64 {
				if tc.email {
					return emailEvents(t, st, source)
				}
				rows, err := st.db.Query(`SELECT id FROM messages WHERE source_id=? AND message_type='calendar_event' ORDER BY id`, source)
				requirements.NoError(err)
				defer func() { _ = rows.Close() }()
				var ids []int64
				for rows.Next() {
					var id int64
					requirements.NoError(rows.Scan(&id))
					ids = append(ids, id)
				}
				requirements.NoError(rows.Err())
				return ids
			}
			if tc.recurrence != "" {
				persist("series", calendarDocument("REQUEST", series))
			}
			emails := func() []string {
				ids := events()
				requirements.NotEmpty(ids)
				tombstone := ids[len(ids)-1]
				if tc.recurrence != "" {
					from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
					page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
					requirements.NoError(err)
					requirements.Len(page.Occurrences, 1)
					assertions.Equal("cancelled", page.Occurrences[0].Status)
					return attendeeEmails(page.Occurrences[0].Attendees)
				}
				stored, err := st.GetMessageRaw(tombstone)
				requirements.NoError(err)
				rows := calendarRows(t, st, tombstone)
				requirements.Len(rows, 1)
				assertions.Contains(string(stored), "UID:family@example.com")
				return attendeeEmails(rows[0].Event.Attendees)
			}
			persist("a", cancel(tc.recurrence, 1, "a@example.com"))
			assertions.Contains(emails(), "a@example.com")
			persist("b", cancel(tc.recurrence, 2, "b@example.com"))
			got := emails()
			assertions.Contains(got, "b@example.com")
			assertions.NotContains(got, "a@example.com")
			ids := events()
			stored, err := st.GetMessageRaw(ids[len(ids)-1])
			requirements.NoError(err)
			assertions.Contains(string(stored), "b@example.com")
			assertions.NotContains(string(stored), "a@example.com")

			// Older and equal revisions leave the newer evidence untouched.
			persist("stale", cancel(tc.recurrence, 1, "a@example.com"))
			persist("equal", cancel(tc.recurrence, 2, "a@example.com"))
			got = emails()
			assertions.Contains(got, "b@example.com")
			assertions.NotContains(got, "a@example.com")
		})
	}
}
