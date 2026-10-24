package calevent

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestICSFixedEasternSummerRejected(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:summer@example.com\r\nDTSTART;TZID=America/New_York:20270702T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	_, err := ParseICS([]byte(raw), "", "UTC")
	require.ErrorContains(t, err, "contradicts America/New_York near DTSTART")
}
