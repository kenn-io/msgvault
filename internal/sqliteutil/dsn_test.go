package sqliteutil

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDSNNormalizesNetURLWindowsPath(t *testing.T) {
	dsn := `file://C:%5CUsers%5Crunner%5Carchive.db?cache=shared`

	normalized, path, err := ResolveDSN(dsn)
	require.NoError(t, err, "ResolveDSN")
	assert.Equal(t, `C:\Users\runner\archive.db`, path)
	assert.Equal(t, `file:///C:/Users/runner/archive.db?cache=shared`, normalized)
}

func TestResolveDSNTreatsLocalhostAuthorityAsLocal(t *testing.T) {
	dsn := `file://localhost/var/lib/msgvault/archive.db?cache=shared`

	normalized, path, err := ResolveDSN(dsn)
	require.NoError(t, err, "ResolveDSN")
	assert.Equal(t, filepath.FromSlash(`/var/lib/msgvault/archive.db`), path)
	assert.Equal(t, dsn, normalized)
}

func TestQueryOnlyDSNRejectsWritesAndKeepsCallerBusyTimeout(t *testing.T) {
	tests := []struct {
		name        string
		options     string
		busyTimeout int
	}{
		{name: "default busy timeout", busyTimeout: 5000},
		{name: "caller busy timeout", options: "?_busy_timeout=100", busyTimeout: 100},
		{name: "caller cannot disable query only", options: "?_query_only=false", busyTimeout: 5000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			writer, err := sql.Open(DriverName(), path)
			require.NoError(err)
			_, err = writer.ExecContext(t.Context(), "CREATE TABLE messages (id INTEGER PRIMARY KEY)")
			require.NoError(err)
			require.NoError(writer.Close())

			dsn, resolvedPath, err := QueryOnlyDSN(path + tt.options)
			require.NoError(err)
			assert.Equal(path, resolvedPath)
			reader, err := sql.Open(DriverName(), dsn)
			require.NoError(err)
			t.Cleanup(func() { _ = reader.Close() })

			var busyTimeout int
			require.NoError(reader.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&busyTimeout))
			assert.Equal(tt.busyTimeout, busyTimeout)
			_, err = reader.ExecContext(t.Context(), "INSERT INTO messages (id) VALUES (1)")
			assert.ErrorContains(err, "readonly")
		})
	}
}

func TestOpenReadOnlyDatabase(t *testing.T) {
	require := require.New(t)

	missingPath := filepath.Join(t.TempDir(), "missing.sqlite")
	_, err := OpenReadOnlyDatabase(context.Background(), missingPath)
	require.Error(err)
	_, err = os.Stat(missingPath)
	require.ErrorIs(err, os.ErrNotExist)

	path := filepath.Join(t.TempDir(), "source # data.sqlite")
	createDSN, err := DatabaseDSN(path, "")
	require.NoError(err)
	db, err := sql.Open("sqlite3", createDSN)
	require.NoError(err)
	defer func() { require.NoError(db.Close()) }()
	var journalMode string
	require.NoError(db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journalMode))
	require.Equal("wal", strings.ToLower(journalMode))
	_, err = db.Exec(`
		CREATE TABLE source_rows (id INTEGER PRIMARY KEY);
		INSERT INTO source_rows (id) VALUES (1);
	`)
	require.NoError(err)

	readOnly, err := OpenReadOnlyDatabase(context.Background(), path)
	require.NoError(err)
	defer func() { require.NoError(readOnly.Close()) }()
	var count int
	require.NoError(readOnly.QueryRow(`SELECT COUNT(*) FROM source_rows`).Scan(&count))
	assert.Equal(t, 1, count)

	_, err = readOnly.Exec(`INSERT INTO source_rows (id) VALUES (2)`)
	require.Error(err)
}
