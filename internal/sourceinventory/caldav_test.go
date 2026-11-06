package sourceinventory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCalDAVInventorySeparatesSelectedCollections(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("caldav", `["owner@example.org","https://calendar.example.org/dav/personal/"]`)
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.CalDAV.Sources = []config.CalDAVSource{
		{Name: "personal", Username: "Owner@Example.org", URL: "https://calendar.example.org/dav/", Calendars: []string{"https://CALENDAR.example.org:443/dav/personal/"}},
		{Name: "work", Username: "owner@example.org", URL: "https://calendar.example.org/dav/", Calendars: []string{"https://calendar.example.org/dav/work/"}},
	}
	items, err := Build(t.Context(), cfg, st)
	require.NoError(err)
	assert.Len(items, 2, "the unimported work collection needs its own inventory item")
	for _, item := range items {
		if item.SourceID != nil {
			assert.Equal(source.ID, *item.SourceID)
			assert.Equal("msgvault sync-caldav personal", item.SyncCommand)
		} else {
			assert.Equal("work", item.Identifier)
			assert.Equal("msgvault sync-caldav work", item.SyncCommand)
		}
	}
}

func TestCalDAVInventoryMatchesCollectionAndMissingAccount(t *testing.T) {
	for _, username := range []string{"owner@example.org", "Owner@Example.org", "calendar-user"} {
		t.Run(username, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			cfg := config.NewDefaultConfig()
			cfg.CalDAV.Sources = []config.CalDAVSource{
				{Name: "personal", Username: username, URL: "https://CALENDAR.example.com:443/dav/"},
				{Name: "work", Username: username, URL: "https://work.example.org/dav/"},
			}
			identity := "owner@example.org"
			if username == "calendar-user" {
				identity = username
			}
			source, err := st.GetOrCreateSource("caldav", `["`+identity+`","https://calendar.example.com/dav/work/"]`)
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
		})
	}
}
