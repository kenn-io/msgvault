package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReviewFutureZoneObservances(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Custom/Test\r\nBEGIN:STANDARD\r\nDTSTART:20271107T020000\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nEND:STANDARD\r\nBEGIN:DAYLIGHT\r\nDTSTART:20270314T020000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:old@example.com\r\nDTSTART;TZID=Custom/Test:20260706T090000\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	raw = []byte(strings.Replace(string(raw), "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:sibling@example.com\r\nDTSTART:20270706T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR", 1))
	raw = []byte(strings.Replace(string(raw), "VERSION:2.0\r\n", "VERSION:2.0\r\nPRODID:-//Example//EN\r\n", 1))
	raw = []byte(strings.ReplaceAll(string(raw), "BEGIN:VEVENT\r\n", "BEGIN:VEVENT\r\nDTSTAMP:20260701T120000Z\r\n"))
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	require.ErrorContains(t, err, "Custom/Test")
	from := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Empty(t, page.Occurrences)
}
