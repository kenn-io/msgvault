package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICalendarBulkCancellationPreservesTypedProjections(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	var ids []int64
	var cancellations strings.Builder
	cancellations.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:CANCEL\r\n")
	for i := range 8 {
		raw := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\n"+
			"BEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n"+
			"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:cancel-%d@example.com\r\n"+
			"DTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=50000\r\n"+
			"ATTENDEE:mailto:guest@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", i)
		summary, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
		requirements.NoError(err)
		requirements.Len(summary.MessageIDs, 1)
		ids = append(ids, summary.MessageIDs[0])
		fmt.Fprintf(&cancellations, "BEGIN:VEVENT\r\nUID:cancel-%d@example.com\r\nSEQUENCE:1\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n", i)
	}
	cancellations.WriteString("END:VCALENDAR\r\n")
	before := calendarRows(t, st, ids...)
	requirements.Len(before, 8)
	for _, phase := range []string{"cancellation", "unchanged replay"} {
		summary, err := st.PersistICalendarContext(t.Context(), source, []byte(cancellations.String()))
		requirements.NoError(err, phase)
		if phase == "cancellation" {
			assertions.Equal(8, summary.Changed)
		} else {
			assertions.Equal(8, summary.Unchanged)
		}
		rows := calendarRows(t, st, ids...)
		requirements.Len(rows, 8)
		for i, row := range rows {
			want := before[i].Event
			want.Status = "cancelled"
			assertions.Equal(want, row.Event, "%s: event %d", phase, i)
		}
		from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err, phase)
		assertions.Empty(page.Unexpandable, phase)
		var listed []int64
		for _, occurrence := range page.Occurrences {
			listed = append(listed, occurrence.MessageID)
			assertions.Equal("cancelled", occurrence.Status, phase)
		}
		assertions.Equal(ids, listed, phase)
	}
	// A newer active revision may have the original event hash. It must still
	// restore the projected status after the metadata-only cancellation.
	id := ids[len(ids)-1]
	stored, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	restored := strings.Replace(string(stored), "BEGIN:VEVENT\r\n", "BEGIN:VEVENT\r\nSEQUENCE:2\r\n", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(restored))
	requirements.NoError(err)
	rows := calendarRows(t, st, id)
	requirements.Len(rows, 1)
	assertions.Equal(before[len(before)-1].Event, rows[0].Event)
}

func TestEmailCalendarRevivalAfterVerificationBudgetExhaustion(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	zone := "BEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\nBEGIN:STANDARD\r\nDTSTART:20260101T000000\r\nRRULE:FREQ=YEARLY;COUNT=2\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n"
	initial := strings.Replace(string(calendarFamilyDocument()), "BEGIN:VEVENT", zone+"BEGIN:VEVENT", 1)
	initial = strings.ReplaceAll(initial, "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\n")
	initial = strings.Replace(initial, "DTSTART:20270303T140000Z", "DTSTART;TZID=Etc/UTC:20270303T140000", 1)
	_, source := persistInviteEmail(t, st, "initial", inviteEmail([]byte(initial)))
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 2)
	before := calendarRows(t, st, ids[1])
	requirements.Len(before, 1)
	master := calendarDocument("REQUEST", "ORGANIZER:mailto:organizer@example.com\r\nSEQUENCE:2\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2")
	persistInviteEmail(t, st, "supersede", inviteEmail(master))
	assertions.Empty(calendarRows(t, st, ids[1]))

	var bulk strings.Builder
	for i := range 11 {
		fmt.Fprintf(&bulk, "BEGIN:VEVENT\r\nUID:work-%d@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270101T090000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=49999\r\nEND:VEVENT\r\n", i)
	}
	exception := "BEGIN:VEVENT" + strings.Split(initial, "BEGIN:VEVENT")[2]
	snapshot := strings.Replace(string(master), "BEGIN:VEVENT", zone+bulk.String()+"BEGIN:VEVENT", 1)
	snapshot = strings.Replace(snapshot, "END:VCALENDAR\r\n", exception, 1)
	for _, phase := range []string{"revival", "unchanged replay"} {
		persistInviteEmail(t, st, phase, inviteEmail([]byte(snapshot)))
		rows := calendarRows(t, st, ids[1])
		requirements.Len(rows, 1, phase)
		assertions.Equal(before[0].Event, rows[0].Event, phase)
		from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err, phase)
		assertions.Empty(page.Unexpandable, phase)
		var revived []CalendarOccurrence
		for _, occurrence := range page.Occurrences {
			if occurrence.MessageID == ids[1] {
				revived = append(revived, occurrence)
			}
		}
		requirements.Len(revived, 1, phase)
		assertions.Equal("2027-03-03T14:00:00Z", revived[0].Start.Format(time.RFC3339), phase)
	}
}
