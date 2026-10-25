package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportICSUsesExistingSourceZone(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource("ics", "tokyo-export")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source.ID, `{"time_zone":"Asia/Tokyo"}`))
	path := filepath.Join(t.TempDir(), "floating.ics")
	raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:tokyo@example.com\r\nDTSTART:20270302T090000\r\nDTEND:20270302T010000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	requirements.NoError(os.WriteFile(path, raw, 0600))
	summary, err := ImportICS(t.Context(), st, path, "tokyo-export", "")
	requirements.NoError(err)
	assertions.Equal(source.ID, summary.SourceID)
	requirements.Len(summary.MessageIDs, 1)
	_, err = ImportICS(t.Context(), st, path, "new-utc-export", "")
	requirements.Error(err)
	_, err = st.GetSourceByTypeAndIdentifier("ics", "new-utc-export")
	assertions.ErrorIs(err, store.ErrSourceNotFound)
}

func TestImportICSTimeZoneFlagSetsSourceZone(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := storetest.New(t).Store
	path := filepath.Join(t.TempDir(), "floating.ics")
	raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:tokyo@example.com\r\nDTSTART:20270302T090000\r\nDTEND:20270302T010000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	requirements.NoError(os.WriteFile(path, raw, 0600))
	_, err := ImportICS(t.Context(), st, path, "zone-export", "Not/AZone")
	requirements.Error(err)
	summary, err := ImportICS(t.Context(), st, path, "zone-export", "Asia/Tokyo")
	requirements.NoError(err)
	requirements.Len(summary.MessageIDs, 1)
	source, err := st.GetSourceByTypeAndIdentifier("ics", "zone-export")
	requirements.NoError(err)
	assertions.JSONEq(`{"time_zone":"Asia/Tokyo"}`, source.SyncConfig.String)
}
