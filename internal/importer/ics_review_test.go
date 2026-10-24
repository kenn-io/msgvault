package importer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportICSMixedCancellationKeepsSibling(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := storetest.New(t).Store
	raw := []byte("BEGIN:VCALENDAR\r\nMETHOD:CANCEL\r\nBEGIN:VEVENT\r\nUID:full@example.com\r\nSTATUS:CANCELLED\r\nDTSTART:20270302T120000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:guest@example.com\r\nATTENDEE:mailto:guest@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	path := filepath.Join(t.TempDir(), "mixed.ics")
	requirements.NoError(os.WriteFile(path, raw, 0600))
	sum, err := ImportICS(t.Context(), st, path, "mixed")
	requirements.NoError(err)
	assertions.Equal(1, sum.Changed)
	requirements.Len(sum.Diagnostics, 1)
	assertions.Contains(sum.Diagnostics[0], "guest removal ignored")
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
}

func TestImportICSIgnoredMethodsLeaveNoSource(t *testing.T) {
	for _, method := range []string{"REPLY", "ADD", "REFRESH"} {
		t.Run(method, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := storetest.New(t).Store
			path := filepath.Join(t.TempDir(), "ignored.ics")
			raw := []byte("BEGIN:VCALENDAR\r\nMETHOD:" + method + "\r\nBEGIN:VEVENT\r\nUID:ignored@example.com\r\nDTSTART:20270302T120000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
			requirements.NoError(os.WriteFile(path, raw, 0600))
			_, err := ImportICS(t.Context(), st, path, "ignored")
			requirements.ErrorContains(err, "no event components")
			var count int
			requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM sources WHERE source_type='ics' AND identifier='ignored'`).Scan(&count))
			assertions.Zero(count)
		})
	}
}
