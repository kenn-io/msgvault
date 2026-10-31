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
	cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal"}, {Name: "work calendar"}}
	_, err := st.GetOrCreateSource("caldav", `["personal","https://calendar.example.com/dav/work/"]`)
	requirements.NoError(err)
	items, err := Build(t.Context(), cfg, st)
	requirements.NoError(err)
	requirements.Len(items, 2)
	commands := map[string]bool{}
	for _, item := range items {
		commands[item.SyncCommand] = true
		assertions.Equal("manual", item.Scheduling)
	}
	assertions.True(commands["msgvault sync-caldav personal"])
	assertions.True(commands["msgvault sync-caldav 'work calendar'"])
}
