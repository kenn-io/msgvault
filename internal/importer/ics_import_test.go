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
	raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:file@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:File event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	path := filepath.Join(t.TempDir(), "calendar.ics")
	requirements.NoError(os.WriteFile(path, raw, 0600))
	first, err := ImportICS(t.Context(), st, path, "example-export")
	requirements.NoError(err)
	requirements.Len(first.MessageIDs, 1)
	assertions.Equal(1, first.Changed)
	stored, err := st.GetMessageRaw(first.MessageIDs[0])
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	requirements.NoError(os.WriteFile(path, []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), 0600))
	_, err = ImportICS(t.Context(), st, path, "example-export")
	requirements.Error(err)
	_, err = ImportICS(t.Context(), st, path, "")
	requirements.Error(err)
}

func TestImportICSFileSizeLimit(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "oversized.ics")
	file, err := os.Create(path)
	requirements.NoError(err)
	requirements.NoError(file.Truncate((32 << 20) + 1))
	requirements.NoError(file.Close())
	_, err = ImportICS(t.Context(), nil, path, "large-export")
	requirements.EqualError(err, "ICS file exceeds 32 MiB limit")
}

func TestImportICSInvalidFileLeavesNoSource(t *testing.T) {
	requirements := require.New(t)
	st := storetest.New(t).Store
	path := filepath.Join(t.TempDir(), "invalid.ics")
	requirements.NoError(os.WriteFile(path, []byte("invalid calendar"), 0600))
	_, err := ImportICS(t.Context(), st, path, "invalid-export")
	requirements.Error(err)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sources WHERE source_type='ics' AND identifier='invalid-export'`).Scan(&count))
	assert.Zero(t, count)
}
