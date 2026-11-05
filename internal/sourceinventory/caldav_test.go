package sourceinventory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCalDAVInventoryMatchesCollectionAndMissingAccount(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.CalDAV.Sources = []config.CalDAVSource{
		{Name: "personal", Username: "owner@example.org", URL: "https://CALENDAR.example.com:443/dav/"},
		{Name: "work", Username: "owner@example.org", URL: "https://work.example.org/dav/"},
	}
	source, err := st.GetOrCreateSource("caldav", `["owner@example.org","https://calendar.example.com/dav/work/"]`)
	requirements.NoError(err)
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 2)
	for _, item := range items {
		assertions.Equal("manual", item.Scheduling)
		if item.SourceID != nil {
			assertions.Equal(source.ID, *item.SourceID)
			assertions.Equal("msgvault sync-caldav personal", item.SyncCommand)
		} else {
			assertions.Equal("work", item.Identifier)
			assertions.Equal("msgvault sync-caldav work", item.SyncCommand)
		}
	}
}
