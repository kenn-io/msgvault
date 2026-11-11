package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportICSRejectsUnreadableFile(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"malformed", "invalid calendar", ""},
		{"unsupported method", "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:ADD\r\nBEGIN:VEVENT\r\nUID:event@example.com\r\nDTSTART:20270302T120000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", `unsupported calendar method "ADD"`},
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
