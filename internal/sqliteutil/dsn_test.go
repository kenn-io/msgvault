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
			requirements := require.New(t)
			assertions := assert.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			writer, err := sql.Open(DriverName(), path)
			requirements.NoError(err)
			_, err = writer.Exec("CREATE TABLE messages (id INTEGER PRIMARY KEY)")
			requirements.NoError(err)
			requirements.NoError(writer.Close())

			dsn, resolvedPath, err := QueryOnlyDSN(path + tt.options)
			requirements.NoError(err)
			assertions.Equal(path, resolvedPath)
			reader, err := sql.Open(DriverName(), dsn)
			requirements.NoError(err)
			t.Cleanup(func() { _ = reader.Close() })

			var busyTimeout int
			requirements.NoError(reader.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout))
			assertions.Equal(tt.busyTimeout, busyTimeout)
			_, err = reader.Exec("INSERT INTO messages (id) VALUES (1)")
			assertions.ErrorContains(err, "readonly")
		})
	}
}

func TestExistingReadWriteDSNPreservesNormalizedFilePaths(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"Windows canonical", `file:///C:/Users/runner/vector%20store.db?cache=shared&mode=rwc`, `file:///C:/Users/runner/vector%20store.db?cache=shared&mode=rw`},
		{"Windows net URL", `file://C:%5CUsers%5Crunner%5Cvector%20store.db?mode=rwc`, `file:///C:/Users/runner/vector%20store.db?mode=rw`},
		{"localhost", `file://localhost/var/lib/vector%20store.db?mode=ro`, `file://localhost/var/lib/vector%20store.db?mode=rw`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, _, err := ExistingReadWriteDSN(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.want, dsn)
		})
	}
}

func TestExistingReadWriteDSNUsesRealExistingWritableFile(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	path := filepath.Join(t.TempDir(), "vector store%.db")
	dsn, resolved, err := ExistingReadWriteDSN(path + "?mode=rwc&_busy_timeout=100")
	requirements.NoError(err)
	assertions.Equal(path, resolved)
	missing, err := sql.Open(DriverName(), dsn)
	requirements.NoError(err)
	requirements.Error(missing.Ping())
	requirements.NoError(missing.Close())
	assertions.NoFileExists(path)
	writer, err := sql.Open(DriverName(), path)
	requirements.NoError(err)
	_, err = writer.Exec(`CREATE TABLE marker (value TEXT)`)
	requirements.NoError(err)
	requirements.NoError(writer.Close())
	existing, err := sql.Open(DriverName(), dsn)
	requirements.NoError(err)
	defer func() { require.NoError(t, existing.Close()) }()
	_, err = existing.Exec(`INSERT INTO marker VALUES ('synthetic')`)
	requirements.NoError(err)
	var value string
	requirements.NoError(existing.QueryRow(`SELECT value FROM marker`).Scan(&value))
	assertions.Equal("synthetic", value)
	var timeout int
	requirements.NoError(existing.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout))
	assertions.Equal(100, timeout)
}
