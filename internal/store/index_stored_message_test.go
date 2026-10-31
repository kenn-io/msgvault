package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
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

	// Restore a missing search entry without changing the stored body.
	clearDocument := `DELETE FROM messages_fts WHERE rowid = ?`
	if st.IsPostgreSQL() {
		clearDocument = `UPDATE messages SET search_fts = NULL WHERE id = ?`
	}
	_, err := st.DB().Exec(st.Rebind(clearDocument), id)
	require.NoError(err)
	ctx := context.Background()
	searchQuery := &search.Query{TextTerms: []string{"orchard"}}
	ids, total, err := st.SearchMessageIDsQueryContext(ctx, searchQuery, 10)
	require.NoError(err)
	assert.Empty(t, ids)
	assert.Zero(t, total)
	require.NoError(st.IndexStoredMessageContext(ctx, id))
	ids, total, err = st.SearchMessageIDsQueryContext(ctx, searchQuery, 10)
	require.NoError(err)
	assert.Equal(t, []int64{id}, ids)
	assert.Equal(t, int64(1), total)
}
