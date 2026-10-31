package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailCalendarRejectsSharedVerificationWork(t *testing.T) {
	for _, tc := range []struct {
		name              string
		count, wantEvents int
	}{
		{"initial parsing", 20, 0},
		{"parsing and retained evidence", 4, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var raw strings.Builder
			raw.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\nBEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n")
			for i := range tc.count {
				fmt.Fprintf(&raw, "BEGIN:VEVENT\r\nUID:work-%d@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=50000\r\nEND:VEVENT\r\n", i)
			}
			raw.WriteString("END:VCALENDAR\r\n")
			parent, source := persistInviteEmail(t, st, "verification-budget", inviteEmail([]byte(raw.String())))
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			assertions.Contains(metadata.String, "budget")
			ids := emailEvents(t, st, source)
			assertions.Len(ids, tc.wantEvents)
			stored, err := st.GetMessageRaw(parent)
			requirements.NoError(err)
			assertions.Equal(inviteEmail([]byte(raw.String())), stored)
			if tc.wantEvents == 0 {
				return
			}
			from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, phase := range []string{"after ingestion", "after unchanged replay"} {
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err, phase)
				assertions.Empty(page.Unexpandable, phase)
				var listed []int64
				for _, occurrence := range page.Occurrences {
					listed = append(listed, occurrence.MessageID)
				}
				assertions.Equal(ids, listed, phase)
				if phase == "after ingestion" {
					requirements.NoError(st.UpsertMessageRawWithFormatContext(t.Context(), parent, stored, "mime"))
				}
			}
		})
	}
}

func TestEmailCalendarRecurrenceEndsShareIngestionBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		verifyZone bool
		knownEnds  int
	}{
		{"UTC", false, 10},
		{"shared with time zone verification", true, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var raw strings.Builder
			raw.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\n")
			if tc.verifyZone {
				raw.WriteString("BEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\nBEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n")
			}
			for i := range 12 {
				count := 49999
				if i == 11 {
					count = 2
				}
				start := ":20270101T090000Z"
				if tc.verifyZone && i == 0 {
					start = ";TZID=Etc/UTC:20270101T090000"
				}
				fmt.Fprintf(&raw, "BEGIN:VEVENT\r\nUID:end-%d@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART%s\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=%d\r\nEND:VEVENT\r\n", i, start, count)
			}
			raw.WriteString("END:VCALENDAR\r\n")
			_, source := persistInviteEmail(t, st, "recurrence-end-budget", inviteEmail([]byte(raw.String())))
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 12)
			for i, id := range ids {
				var end sql.NullTime
				requirements.NoError(st.db.QueryRow(`SELECT recurrence_end_at FROM calendar_events WHERE message_id=?`, id).Scan(&end))
				assertions.Equal(i < tc.knownEnds, end.Valid, "event %d", i)
			}
			from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			assertions.Empty(page.Unexpandable)
			assertions.Len(page.Occurrences, 12, "unknown recurrence ends must remain available for listing")
		})
	}
}
