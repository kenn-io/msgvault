package sourceinventory

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCalDAVInventoryMatchesCollectionAndMissingAccount(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal"}, {Name: "work calendar"}}
	_, err := st.GetOrCreateSource("caldav", `["personal","https://calendar.example.com/dav/work/"]`)
	require.NoError(err)
	items, err := Build(t.Context(), cfg, st)
	require.NoError(err)
	require.Len(items, 2)
	commands := map[string]bool{}
	for _, item := range items {
		commands[item.SyncCommand] = true
		assert.Equal("manual", item.Scheduling)
	}
	assert.True(commands["msgvault sync-caldav personal"])
	assert.True(commands["msgvault sync-caldav 'work calendar'"])
}
