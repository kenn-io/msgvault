package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICalendarUnsupportedZonedRecurrenceKeepsSibling(t *testing.T) {
	for _, rule := range []string{
		"FREQ=HOURLY;COUNT=3",
		"FREQ=HOURLY;COUNT=50001",
		"FREQ=DAILY;COUNT=3;BYHOUR=9,10",
		"FREQ=SECONDLY;INTERVAL=2;BYSECOND=1;COUNT=1",
		"FREQ=DAILY;INTERVAL=7;BYDAY=MO;COUNT=1",
	} {
		t.Run(rule, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
				"BEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
				"BEGIN:VEVENT\r\nUID:unsupported@example.com\r\nDTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1H\r\nRRULE:" + rule + "\r\nEND:VEVENT\r\n" +
				"BEGIN:VEVENT\r\nUID:sibling@example.com\r\nDTSTART;TZID=Etc/UTC:20270101T120000\r\nDURATION:PT1H\r\nSUMMARY:Readable sibling\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			for _, phase := range []string{"import", "unchanged replay"} {
				summary, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
				requirements.NoError(err, phase)
				requirements.Len(summary.MessageIDs, 2, phase)
				assertions.Equal(1, summary.Unexpandable, phase)
				stored, err := st.GetMessageRaw(summary.MessageIDs[0])
				requirements.NoError(err)
				assertions.Contains(string(stored), "RRULE:"+rule)
				assertions.NotContains(string(stored), "UID:sibling@example.com")
				from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err, phase)
				requirements.Len(page.Unexpandable, 1, phase)
				assertions.Equal(summary.MessageIDs[0], page.Unexpandable[0].MessageID, phase)
				assertions.Contains(page.Unexpandable[0].Reason, "unsupported recurrence", phase)
				requirements.Len(page.Occurrences, 1, phase)
				assertions.Equal(summary.MessageIDs[1], page.Occurrences[0].MessageID, phase)
				assertions.Equal("Readable sibling", page.Occurrences[0].Subject, phase)
				assertions.Equal("2027-01-01T12:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339), phase)
			}
		})
	}
}
