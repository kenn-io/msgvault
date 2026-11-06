package cmd

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
)

func TestCacheSnapshotIncrementalSliceParallelReaders(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := sql.Open("sqlite3", path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(source.Close()) })
	_, err = source.Exec(`
		CREATE TABLE message_recipients (
			id INTEGER PRIMARY KEY, message_id INTEGER, participant_id INTEGER,
			recipient_type TEXT
		);
		WITH RECURSIVE ids(id) AS (
			SELECT 1 UNION ALL SELECT id + 1 FROM ids WHERE id < 20002
		)
		INSERT INTO message_recipients
		SELECT id, id, id, 'from' FROM ids
	`)
	require.NoError(err)
	duck, err := duckdbutil.Open(t.Context(), duckdbutil.BuilderPolicyWithOverrides(
		t.TempDir(), duckdbutil.BuilderOverrides{Threads: 4},
	))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(duck.Close()) })
	requireSQLiteScanner(t, duck)
	snapshot, err := openCacheSourceSnapshot(duck, path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(snapshot.Close()) })
	if snapshot.csvSnapshot {
		t.Skip("requires the SQLite scanner")
	}
	relation, err := snapshot.messageSliceRelation("message_recipients", "message_id", 1, 20001)
	require.NoError(err)

	// The messages export reads the recipient slice in several subqueries.
	// DuckDB can schedule those readers on different worker threads.
	var count, leftSum, rightSum, thirdSum int64
	err = snapshot.DuckDB().QueryRow(fmt.Sprintf(`
		SELECT COUNT(*), SUM(a.participant_id), SUM(b.participant_id), SUM(c.participant_id)
		FROM %s a
		JOIN %s b ON a.message_id = b.message_id
		JOIN %s c ON a.message_id = c.message_id
	`, relation, relation, relation)).Scan(&count, &leftSum, &rightSum, &thirdSum)
	require.NoError(err)
	assert.Equal(int64(20000), count)
	assert.Equal(int64(200030000), leftSum)
	assert.Equal(int64(200030000), rightSum)
	assert.Equal(int64(200030000), thirdSum)
}
