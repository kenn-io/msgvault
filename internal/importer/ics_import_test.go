package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportICSRejectsUnreadableFile(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"malformed", "invalid calendar", ""},
		{"unsupported method", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:ADD\r\nBEGIN:VEVENT\r\nUID:event@example.com\r\nDTSTART:20270302T120000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", `unsupported calendar method "ADD"`},
		{"unsupported zone", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Custom/Test\r\nBEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nRDATE:20270301T020000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:invalid@example.com\r\nDTSTART;TZID=Custom/Test:20270302T120000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", "unsupported VTIMEZONE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			path := filepath.Join(t.TempDir(), "invalid.ics")
			requirements.NoError(os.WriteFile(path, []byte(tc.raw), 0600))
			_, err := ImportICS(t.Context(), st, path, "invalid-export")
			requirements.Error(err)
			if tc.want != "" {
				requirements.ErrorContains(err, tc.want)
			}
		})
	}
}

func TestImportICSRangeKeepsOtherEvents(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "calendar.ics")
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nBEGIN:VEVENT\r\nUID:series@example.com\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=10\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:series@example.com\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:sibling@example.com\r\nDTSTART:20270308T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Sibling\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw = strings.ReplaceAll(raw, "BEGIN:VEVENT\r\n", "BEGIN:VEVENT\r\nDTSTAMP:20270301T120000Z\r\n")
	requirements.NoError(os.WriteFile(path, []byte(raw), 0600))
	summary, err := ImportICS(t.Context(), st, path, "range-export")
	requirements.NoError(err)
	assertions.Equal(3, summary.Changed)
	from := time.Date(2027, 3, 8, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("Sibling", page.Occurrences[0].Subject)
	requirements.Len(page.Unexpandable, 1)
}
