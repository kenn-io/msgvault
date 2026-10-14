package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
)

// ensureIdentityCacheFixtureDatasets keeps hand-built API fixtures compatible
// with the current cache contract by deriving the identity datasets from
// the fixture's raw Parquet tables with the production builder. The derived
// relationship_activity dataset is a live dependency of the people, domain,
// participant-grouping, and timeline read paths, so an empty stand-in would
// make those queries return no rows.
func ensureIdentityCacheFixtureDatasets(
	tb testing.TB,
	db *sql.DB,
	analyticsDir string,
) {
	tb.Helper()
	for _, dataset := range []struct {
		name, query string
	}{
		{name: "room_addresses", query: `SELECT ''::VARCHAR AS email WHERE false`},
		{name: "person_contacts", query: `SELECT 0::BIGINT AS participant_id, ''::VARCHAR AS address_kind, ''::VARCHAR AS normalized_value WHERE false`},
	} {
		path := filepath.Join(analyticsDir, dataset.name, dataset.name+".parquet")
		_, statErr := os.Stat(path)
		if statErr == nil {
			continue
		}
		require.True(tb, os.IsNotExist(statErr), "inspect %s fixture dataset", dataset.name)
		require.NoError(tb, os.MkdirAll(filepath.Dir(path), 0o755), "create %s fixture directory", dataset.name)
		_, err := db.Exec(fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET)", dataset.query, filepath.ToSlash(path)))
		require.NoError(tb, err, "write empty %s fixture dataset", dataset.name)
	}
	ensureMessageMeetingKeys(tb, db, analyticsDir)
	personDisplayNamesDir := filepath.Join(analyticsDir, "person_display_names")
	require.NoError(tb, os.MkdirAll(personDisplayNamesDir, 0o755), "create person_display_names fixture directory")
	personDisplayNamesPath := filepath.ToSlash(filepath.Join(personDisplayNamesDir, "person_display_names.parquet"))
	_, err := db.Exec(fmt.Sprintf(
		"COPY (SELECT 0::BIGINT AS participant_id, 0::BIGINT AS person_id, ''::VARCHAR AS display_name WHERE false) TO '%s' (FORMAT PARQUET)",
		personDisplayNamesPath,
	))
	require.NoError(tb, err, "write empty person_display_names fixture dataset")
	_, err = identityindex.Build(context.Background(), db, identityindex.BuildOptions{
		Mode:           identityindex.ModeFull,
		StagedBaseRoot: analyticsDir,
		OutputRoot:     analyticsDir,
	})
	require.NoError(tb, err, "derive identity fixture datasets")
}

func ensureMessageMeetingKeys(tb testing.TB, db *sql.DB, analyticsDir string) {
	tb.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(analyticsDir, "messages"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".parquet") {
			files = append(files, path)
		}
		return nil
	})
	require.NoError(tb, err, "find messages fixture shards")
	for _, path := range files {
		quotedPath := strings.ReplaceAll(filepath.ToSlash(path), "'", "''")
		names := func() []string {
			columns, err := db.Query(fmt.Sprintf("SELECT * FROM read_parquet('%s') LIMIT 0", quotedPath))
			require.NoError(tb, err, "inspect messages fixture shard")
			defer func() { require.NoError(tb, columns.Close()) }()
			names, err := columns.Columns()
			require.NoError(tb, err, "read messages fixture columns")
			require.NoError(tb, columns.Err(), "read messages fixture columns")
			return names
		}()
		if slices.Contains(names, "meeting_key") {
			continue
		}
		temporaryPath := path + ".with-meeting-key"
		quotedTemporaryPath := strings.ReplaceAll(filepath.ToSlash(temporaryPath), "'", "''")
		_, err = db.Exec(fmt.Sprintf("COPY (SELECT *, CASE WHEN message_type='meeting_transcript' THEN 'msg:' || CAST(id AS VARCHAR) ELSE NULL::VARCHAR END AS meeting_key FROM read_parquet('%s')) TO '%s' (FORMAT PARQUET)", quotedPath, quotedTemporaryPath))
		require.NoError(tb, err, "add meeting keys to messages fixture shard")
		require.NoError(tb, os.Rename(temporaryPath, path), "replace messages fixture shard with meeting keys")
	}
}
