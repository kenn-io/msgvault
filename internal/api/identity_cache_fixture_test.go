package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
)

// ensureIdentityCacheFixtureDatasets keeps hand-built API fixtures compatible
// with the version-15 cache contract by deriving the identity datasets from
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
	_, err := identityindex.Build(context.Background(), db, identityindex.BuildOptions{
		Mode:           identityindex.ModeFull,
		StagedBaseRoot: analyticsDir,
		OutputRoot:     analyticsDir,
	})
	require.NoError(t, err, "derive identity fixture datasets")
}
