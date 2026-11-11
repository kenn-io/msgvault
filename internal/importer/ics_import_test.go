package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestImportICSOversizedEventLeavesNoSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "oversized.ics")
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:large@example.com\r\nDTSTART:20270302T120000Z\r\nDESCRIPTION:" + strings.Repeat("a", 64<<20) + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
	_, err := ImportICS(t.Context(), st, path, "large-export")
	require.ErrorContains(t, err, "retained calendar exceeds")
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), "ics", "large-export")
	assert.ErrorIs(t, err, store.ErrSourceNotFound)
}
