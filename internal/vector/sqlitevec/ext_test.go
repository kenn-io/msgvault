//go:build sqlite_vec

package sqlitevec

import (
	"database/sql"
	"testing"

	requirepkg "github.com/stretchr/testify/require"
)

func TestSQLiteVecExtensionLoads(t *testing.T) {
	require := requirepkg.New(t)
	require.NoError(RegisterExtension(), "RegisterExtension")
	db, err := sql.Open(DriverName(), ":memory:")
	require.NoError(err, "open")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE VIRTUAL TABLE t USING vec0(
		generation_id INTEGER PARTITION KEY,
		message_id INTEGER PRIMARY KEY,
		embedding FLOAT[4]
	)`)
	require.NoError(err, "create virtual table")

	// Sanity: insert and query a vector.
	// Little-endian float32 blob for [1.0, 0.0, 0.0, 0.0].
	_, err = db.Exec(`INSERT INTO t (generation_id, message_id, embedding) VALUES (?, ?, ?)`,
		1, 42, []byte{0, 0, 0x80, 0x3f, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	require.NoError(err, "insert vector")
}
