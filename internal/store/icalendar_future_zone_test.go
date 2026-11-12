package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectedCalendarDoesNotImportSibling(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nBEGIN:VTIMEZONE\r\nTZID:Custom/Test\r\nBEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:sibling@example.com\r\nDTSTAMP:20260701T120000Z\r\nDTSTART:20270706T090000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:old@example.com\r\nDTSTAMP:20260701T120000Z\r\nDTSTART;TZID=Custom/Test:20260706T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	_, err := st.PersistICalendarContext(t.Context(), source, raw)
	require.ErrorContains(t, err, "Custom/Test")
	var count int
	require.NoError(t, st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE source_id=?`, source).Scan(&count))
	require.Zero(t, count)
}
