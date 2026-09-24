package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheTextRepairsSanitize(t *testing.T) {
	tests := []struct {
		name, in, want string
		counted        int64
	}{
		{"valid text is unchanged", "Müller 🎉", "Müller 🎉", 0},
		{"lone continuation byte", "\x80", "�", 1},
		{"truncated emoji", "Calendar: lunch \xf0\x9f", "Calendar: lunch ��", 1},
		{"following valid bytes survive", "é\xe9té", "é�té", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repairs := &cacheTextRepairs{}
			assert.Equal(t, tt.want, repairs.sanitize(tt.in))
			assert.Equal(t, tt.counted, repairs.Count())
		})
	}
	var nilRepairs *cacheTextRepairs
	assert.Equal(t, int64(0), nilRepairs.Count())
}

func requireSQLiteScanner(t *testing.T, duckDB *sql.DB) {
	t.Helper()
	if _, err := duckDB.Exec("INSTALL sqlite; LOAD sqlite;"); err != nil {
		t.Skipf("DuckDB sqlite extension unavailable: %v", err)
	}
}

func TestCacheIdentityTextSQLValidOrNull(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	sqlitePath := filepath.Join(t.TempDir(), "identity.db")
	sqliteDB, err := sql.Open("sqlite3", sqlitePath)
	require.NoError(err)
	_, err = sqliteDB.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	require.NoError(err)
	for id, raw := range map[int][]byte{
		1: []byte("ok"),
		2: []byte("a\x80b"),
		3: []byte("\x80"),
		4: []byte("primary\uFFFD@example.com"),
	} {
		_, err = sqliteDB.Exec(`INSERT INTO t VALUES (?, CAST(? AS TEXT))`, id, raw)
		require.NoError(err)
	}
	_, err = sqliteDB.Exec(`INSERT INTO t VALUES (5, NULL)`)
	require.NoError(err)
	require.NoError(sqliteDB.Close())

	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckDB.Close() }()
	duckDB.SetMaxOpenConns(1)
	requireSQLiteScanner(t, duckDB)
	_, err = duckDB.Exec(fmt.Sprintf("ATTACH '%s' AS src (TYPE sqlite, READ_ONLY)",
		strings.ReplaceAll(sqlitePath, "'", "''")))
	require.NoError(err)

	// cacheIdentityTextSQL is pure SQL: it needs no registered fallback
	// function, only the sqlite scanner passing stored bytes through.
	rows, err := duckDB.Query(`SELECT id, ` + cacheIdentityTextSQL("v") + ` FROM src.t ORDER BY id`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[int]sql.NullString{}
	for rows.Next() {
		var id int
		var v sql.NullString
		require.NoError(rows.Scan(&id, &v))
		got[id] = v
	}
	require.NoError(rows.Err())

	assert.Equal(sql.NullString{String: "ok", Valid: true}, got[1], "valid value passes through unchanged")
	assert.Equal(sql.NullString{}, got[2], "invalid value becomes NULL")
	assert.Equal(sql.NullString{}, got[3], "invalid value becomes NULL")
	assert.Equal(sql.NullString{String: "primary\uFFFD@example.com", Valid: true}, got[4],
		"a literal U+FFFD rune is valid UTF-8 and passes through byte-identical")
	assert.Equal(sql.NullString{}, got[5], "NULL stays NULL")
}

func TestCacheEnvelopePresenceSQLMatchesSQLiteTrimSemantics(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	cases := map[int][]byte{
		1: []byte("owner@example.com"),
		2: []byte("owner\x80@example.com"),
		3: []byte("\x80"),
		4: []byte("   "),
		5: []byte(""),
		6: []byte("owner\uFFFD@example.com"),
	}
	sqlitePath := filepath.Join(t.TempDir(), "envelope.db")
	sqliteDB, err := sql.Open("sqlite3", sqlitePath)
	require.NoError(err)
	_, err = sqliteDB.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	require.NoError(err)
	for id, raw := range cases {
		_, err = sqliteDB.Exec(`INSERT INTO t VALUES (?, CAST(? AS TEXT))`, id, raw)
		require.NoError(err)
	}
	_, err = sqliteDB.Exec(`INSERT INTO t VALUES (7, NULL)`)
	require.NoError(err)

	// The store decides envelope presence with SQLite over the raw bytes;
	// the cache expression must agree row for row.
	want := map[int]bool{}
	storeRows, err := sqliteDB.Query(`SELECT id, v IS NOT NULL AND TRIM(v) <> '' FROM t`)
	require.NoError(err)
	defer func() { require.NoError(storeRows.Close()) }()
	for storeRows.Next() {
		var id int
		var present bool
		require.NoError(storeRows.Scan(&id, &present))
		want[id] = present
	}
	require.NoError(storeRows.Err())
	require.NoError(sqliteDB.Close())

	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckDB.Close() }()
	duckDB.SetMaxOpenConns(1)
	requireSQLiteScanner(t, duckDB)
	_, err = duckDB.Exec(fmt.Sprintf("ATTACH '%s' AS src (TYPE sqlite, READ_ONLY)",
		strings.ReplaceAll(sqlitePath, "'", "''")))
	require.NoError(err)

	rows, err := duckDB.Query(`SELECT id, ` + cacheEnvelopePresenceSQL("v") + ` FROM src.t ORDER BY id`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[int]bool{}
	for rows.Next() {
		var id int
		var present bool
		require.NoError(rows.Scan(&id, &present))
		got[id] = present
	}
	require.NoError(rows.Err())

	for id := range want {
		assert.Equal(want[id], got[id],
			"row %d: cache envelope presence must match the store's raw-column guard", id)
	}
	assert.True(got[2], "invalid bytes are still a recorded envelope")
	assert.True(got[3], "a single invalid byte is still a recorded envelope")
	assert.False(got[4], "an all-spaces envelope counts as absent, like SQLite TRIM")
	assert.False(got[5], "an empty envelope counts as absent")
	assert.False(got[7], "a NULL envelope counts as absent")
}

func TestCacheTextSQLRepairsSQLiteScannerValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	sqlitePath := filepath.Join(t.TempDir(), "text.db")
	sqliteDB, err := sql.Open("sqlite3", sqlitePath)
	require.NoError(err)
	_, err = sqliteDB.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	require.NoError(err)
	for id, raw := range map[int][]byte{1: []byte("ok"), 2: []byte("ab\xf0\x9fcd"), 3: []byte("\x80")} {
		_, err = sqliteDB.Exec(`INSERT INTO t VALUES (?, CAST(? AS TEXT))`, id, raw)
		require.NoError(err)
	}
	_, err = sqliteDB.Exec(`INSERT INTO t VALUES (4, NULL)`)
	require.NoError(err)
	require.NoError(sqliteDB.Close())

	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckDB.Close() }()
	duckDB.SetMaxOpenConns(1)
	repairs := &cacheTextRepairs{}
	require.NoError(registerCacheTextFunctions(ctx, duckDB, repairs))
	requireSQLiteScanner(t, duckDB)
	_, err = duckDB.Exec(fmt.Sprintf("ATTACH '%s' AS src (TYPE sqlite, READ_ONLY)",
		strings.ReplaceAll(sqlitePath, "'", "''")))
	require.NoError(err)

	rows, err := duckDB.Query(`SELECT id, ` + cacheTextSQL("v") + ` FROM src.t ORDER BY id`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[int]sql.NullString{}
	for rows.Next() {
		var id int
		var v sql.NullString
		require.NoError(rows.Scan(&id, &v))
		got[id] = v
	}
	require.NoError(rows.Err())

	assert.Equal(sql.NullString{String: "ok", Valid: true}, got[1])
	assert.Equal(sql.NullString{String: "ab��cd", Valid: true}, got[2])
	assert.Equal(sql.NullString{String: "�", Valid: true}, got[3])
	assert.Equal(sql.NullString{}, got[4], "NULL must stay NULL")
	assert.Equal(int64(2), repairs.Count(), "only invalid values reach the Go fallback")
}
