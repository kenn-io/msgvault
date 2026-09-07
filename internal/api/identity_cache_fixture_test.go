package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	t *testing.T,
	db *sql.DB,
	analyticsDir string,
) {
	t.Helper()
	personDisplayNamesDir := filepath.Join(analyticsDir, "person_display_names")
	require.NoError(t, os.MkdirAll(personDisplayNamesDir, 0o755), "create person_display_names fixture directory")
	personDisplayNamesPath := filepath.ToSlash(filepath.Join(personDisplayNamesDir, "person_display_names.parquet"))
	_, err := db.Exec(fmt.Sprintf(
		"COPY (SELECT 0::BIGINT AS participant_id, 0::BIGINT AS person_id, ''::VARCHAR AS display_name WHERE false) TO '%s' (FORMAT PARQUET)",
		personDisplayNamesPath,
	))
	require.NoError(t, err, "write empty person_display_names fixture dataset")
	_, err = identityindex.Build(context.Background(), db, identityindex.BuildOptions{
		Mode:           identityindex.ModeFull,
		StagedBaseRoot: analyticsDir,
		OutputRoot:     analyticsDir,
	})
	require.NoError(t, err, "derive identity fixture datasets")
}
