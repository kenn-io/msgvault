//go:build sqlite_vec

package vec1

import (
	"database/sql"
	"encoding/binary"
	"math"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoRegistersVec1OnSQLiteConnections(t *testing.T) {
	Auto()

	db, err := sql.Open("sqlite3", t.TempDir()+"/vec1.db")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var version string
	require.NoError(t, db.QueryRow(`SELECT vec1_info()`).Scan(&version))
	assert.Contains(t, version, "version "+Version)

	_, err = db.Exec(`CREATE VIRTUAL TABLE items USING vec1(embedding)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO items(rowid, embedding) VALUES (?, ?), (?, ?)`,
		int64(7), float32Blob(1, 0), int64(9), float32Blob(0, 1))
	require.NoError(t, err)

	var rowID int64
	require.NoError(t, db.QueryRow(
		`SELECT rowid FROM items(?) LIMIT 1`, float32Blob(0.9, 0.1)).Scan(&rowID))
	assert.Equal(t, int64(7), rowID)
}

func float32Blob(values ...float32) []byte {
	blob := make([]byte, len(values)*4)
	for i, value := range values {
		binary.NativeEndian.PutUint32(blob[i*4:], math.Float32bits(value))
	}
	return blob
}
