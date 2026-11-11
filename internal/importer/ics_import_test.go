package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportICSRejectsUnsupportedMethod(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "invalid.ics")
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:ADD\r\nBEGIN:VEVENT\r\nUID:event@example.com\r\nDTSTART:20270302T120000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	requirements.NoError(os.WriteFile(path, []byte(raw), 0600))
	_, err := ImportICS(t.Context(), st, path, "invalid-export")
	requirements.ErrorContains(err, `unsupported calendar method "ADD"`)
}
