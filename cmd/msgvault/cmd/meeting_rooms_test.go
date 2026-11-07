package cmd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportedMeetingRosterExcludesRoomGroups(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	root := t.TempDir()
	dbPath := filepath.Join(root, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("granola", "archive@example.com")
	requirements.NoError(err)
	_, err = meetingarchive.New(st).Upsert(t.Context(), meetingarchive.Snapshot{
		SourceID: source.ID, AccountEmail: "archive@example.com", SourceMessageID: "meeting-room",
		Title: "Planning", StartedAt: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
		Organizer: &meetingarchive.Person{Name: "Alice", Email: "alice@example.com"},
		Attendees: []meetingarchive.Person{{Name: "Room", Email: "room@resource.calendar.google.com"}},
	}, meetingarchive.UpsertOptions{})
	requirements.NoError(err)
	analyticsDir := filepath.Join(root, "analytics")
	_, err = buildCache(t.Context(), dbPath, analyticsDir, true)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(t.Context(), analyticsDir, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	for _, dimension := range []string{"participant", "domain"} {
		groups, err := engine.ExploreGroups(t.Context(), query.ExploreGroupRequest{Dimension: dimension})
		requirements.NoError(err)
		requirements.Len(groups.Rows, 1, dimension)
		assertions.Equal(int64(1), groups.Rows[0].Count)
	}
}
