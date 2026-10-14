package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportICSFile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:file@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Example file event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	path := filepath.Join(t.TempDir(), "calendar.ics")
	requirements.NoError(os.WriteFile(path, raw, 0600))
	first, err := ImportICS(t.Context(), st, path, "example-export")
	requirements.NoError(err)
	requirements.Len(first.MessageIDs, 1)
	assertions.Equal(1, first.Changed)
	second, err := ImportICS(t.Context(), st, path, "example-export")
	requirements.NoError(err)
	assertions.Equal(first.MessageIDs, second.MessageIDs)
	assertions.Equal(1, second.Unchanged)
	stored, err := st.GetMessageRaw(first.MessageIDs[0])
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	requirements.NoError(os.WriteFile(path, []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), 0600))
	_, err = ImportICS(t.Context(), st, path, "example-export")
	requirements.Error(err)
	_, err = ImportICS(t.Context(), st, path, "")
	requirements.Error(err)
}
