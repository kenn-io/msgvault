package query_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/tiktokexport"
)

func TestTikTokExportVisibleAsTextConversations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// TextEngine is a SQLite reader; importer persistence also runs on PostgreSQL.
	st := testutil.NewSQLiteTestStore(t)
	_, err := tiktokexport.Import(t.Context(), st, "example-account", "../tiktokexport/testdata/json")
	require.NoError(err)
	rows, err := query.NewSQLiteEngine(st.DB()).ListConversations(t.Context(), query.TextFilter{})
	require.NoError(err)
	require.Len(rows, 2)
	assert.True(query.IsKnownMessageType(tiktokexport.SourceType))
	assert.True(query.IsChatEntry(tiktokexport.SourceType, "public_comments"))
	var count int64
	for _, row := range rows {
		count += row.MessageCount
	}
	assert.Equal(int64(4), count)
}
