package sqliteutil

import (
	"database/sql"
	"path/filepath"
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
			_, err = writer.Exec("CREATE TABLE messages (id INTEGER PRIMARY KEY)")
			require.NoError(err)
			require.NoError(writer.Close())

			dsn, resolvedPath, err := QueryOnlyDSN(path + tt.options)
			require.NoError(err)
			assert.Equal(path, resolvedPath)
			reader, err := sql.Open(DriverName(), dsn)
			require.NoError(err)
			t.Cleanup(func() { _ = reader.Close() })

			var busyTimeout int
			require.NoError(reader.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout))
			assert.Equal(tt.busyTimeout, busyTimeout)
			_, err = reader.Exec("INSERT INTO messages (id) VALUES (1)")
			assert.ErrorContains(err, "readonly")
		})
	}
}
