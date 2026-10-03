package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopySubsetKeepsMatrixCiphertextForCopiedEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")
	srcDB := createTestSourceDB(t, srcDir, 2)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`INSERT INTO matrix_encrypted_events (source_id, event_id, room_id, raw_event)
		VALUES (1, 'msg_1', '!room:example.org', X'7B7D'),
		       (1, 'msg_2', '!room:example.org', X'7B7D'),
		       (1, 'unrelated_event', '!room:example.org', X'7B7D')`)
	require.NoError(err)
	require.NoError(db.Close())

	_, err = CopySubset(srcDB, dstDir, 1, false)
	require.NoError(err)
	destination, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { require.NoError(destination.Close()) }()
	rows, err := destination.Query(`SELECT event_id FROM matrix_encrypted_events ORDER BY event_id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var eventIDs []string
	for rows.Next() {
		var eventID string
		require.NoError(rows.Scan(&eventID))
		eventIDs = append(eventIDs, eventID)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"msg_2"}, eventIDs)
}
