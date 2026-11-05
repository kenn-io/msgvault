package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportICSRejectsInvalidFile(t *testing.T) {
	requirements := require.New(t)
	st, dir := openTestStore(t)
	path := filepath.Join(dir, "invalid.ics")
	requirements.NoError(os.WriteFile(path, []byte("invalid calendar"), 0o600))
	_, err := ImportICS(t.Context(), st, path, "invalid-calendar")
	requirements.ErrorContains(err, "decode iCalendar:")
}
