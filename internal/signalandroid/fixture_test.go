package signalandroid

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeTablesFixture writes a SQLite file containing only the given DDL.
func writeTablesFixture(t *testing.T, ddl ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(strings.Join(ddl, ";\n"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "DatabaseVersion.sbf"), []byte("VERSION:uint32:215\n"), 0o600))
	return path
}
