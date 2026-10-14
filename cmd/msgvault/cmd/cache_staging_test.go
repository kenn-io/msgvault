package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/store"
)

func TestCacheStagingTablesAreVisibleAcrossDuckDBConnections(t *testing.T) {
	requirements := require.New(t)
	t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")

	archivePath := filepath.Join(t.TempDir(), "archive.db")
	archive, err := store.OpenForTest(archivePath)
	requirements.NoError(err)
	requirements.NoError(archive.InitSchema())
	t.Cleanup(func() { requirements.NoError(archive.Close()) })

	duckDB, err := sql.Open("duckdb", filepath.Join(t.TempDir(), "builder.duckdb"))
	requirements.NoError(err)
	duckDB.SetMaxOpenConns(2)
	duckDB.SetMaxIdleConns(2)
	requirements.NoError(duckdbutil.Apply(t.Context(), duckDB, duckdbutil.BuilderPolicy(filepath.Join(t.TempDir(), "spill"))))
	t.Cleanup(func() { requirements.NoError(duckDB.Close()) })

	snapshot, err := openCacheSourceSnapshot(duckDB, archivePath)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(snapshot.Close()) })

	pinned, err := duckDB.Conn(t.Context())
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(pinned.Close()) })

	requirements.NoError(stageCacheMeetingKeys(t.Context(), snapshot, archivePath, 0, 0))
	requirements.NoError(stageCachePeopleRooms(t.Context(), snapshot))
	requirements.NoError(stageParticipantClusters(t.Context(), snapshot.DuckDB(), map[int64]int64{7: 7}))

	for _, table := range []struct {
		name     string
		wantRows int
	}{
		{name: "cache_meeting_keys"},
		{name: "cache_room_addresses"},
		{name: "cache_person_contacts"},
		{name: "tmp_participant_clusters", wantRows: 1},
	} {
		t.Run(table.name, func(t *testing.T) {
			var count int
			err := pinned.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table.name).Scan(&count)
			require.NoError(t, err)
			assert.Equal(t, table.wantRows, count)
		})
	}
}
