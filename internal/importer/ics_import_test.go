package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportICSMalformedFileLeavesNoSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "invalid.ics")
	require.NoError(t, os.WriteFile(path, []byte("invalid calendar"), 0600))
	_, err := ImportICS(t.Context(), st, path, "invalid-export")
	require.Error(t, err)
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), "ics", "invalid-export")
	assert.ErrorIs(t, err, store.ErrSourceNotFound)
}

func TestImportICSRangeKeepsOtherEvents(t *testing.T) {
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "calendar.ics")
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nBEGIN:VEVENT\r\nUID:series@example.com\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=10\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:series@example.com\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:sibling@example.com\r\nDTSTART:20270308T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Sibling\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw = strings.ReplaceAll(raw, "BEGIN:VEVENT\r\n", "BEGIN:VEVENT\r\nDTSTAMP:20270301T120000Z\r\n")
	require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
	summary, err := ImportICS(t.Context(), st, path, "range-export")
	require.NoError(t, err)
	assert.Equal(t, 3, summary.Changed)
	from := time.Date(2027, 3, 8, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Len(t, page.Occurrences, 1)
	assert.Equal(t, "Sibling", page.Occurrences[0].Subject)
	require.Len(t, page.Unexpandable, 1)
}

func TestImportICSMultipleEventsOverFileLimit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "large-calendar.ics")
	padding := strings.Repeat("a", 32<<20)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nBEGIN:VEVENT\r\nUID:first@example.com\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T120000Z\r\nX-PADDING:" + padding + "\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:second@example.com\r\nDTSTAMP:20270301T120000Z\r\nDTSTART:20270302T130000Z\r\nX-PADDING:" + padding + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	requirements.NoError(os.WriteFile(path, []byte(raw), 0600))
	summary, err := ImportICS(t.Context(), st, path, "large-export")
	requirements.NoError(err)
	assertions.Equal(2, summary.Changed)
}
