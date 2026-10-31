package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIndexStoredMessageLeavesUnchangedEntry(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	if !st.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	id := f.CreateMessage("unchanged")
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "orchard", Valid: true}, sql.NullString{}))
	require.NoError(st.IndexStoredMessageContext(context.Background(), id))
	// Observe database writes, so replacing an identical search row fails this test.
	query := `SELECT total_changes()`
	var args []any
	if st.IsPostgreSQL() {
		query = `SELECT xmin::text FROM messages WHERE id = $1`
		args = []any{id}
	} else {
		st.DB().SetMaxOpenConns(1)
	}
	var before, after string
	require.NoError(st.DB().QueryRow(query, args...).Scan(&before))
	require.NoError(st.IndexStoredMessageContext(context.Background(), id))
	require.NoError(st.DB().QueryRow(query, args...).Scan(&after))
	assert.Equal(t, before, after)
}
