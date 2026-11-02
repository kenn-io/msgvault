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
	st := testutil.NewTestStore(t)
	_, err := tiktokexport.Import(t.Context(), st, "example-account", "../tiktokexport/testdata/json")
	require.NoError(t, err)
	rows, err := query.NewSQLiteEngine(st.DB()).ListConversations(t.Context(), query.TextFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.True(t, query.IsKnownMessageType(tiktokexport.SourceType))
	assert.True(t, query.IsChatEntry(tiktokexport.SourceType, "public_comments"))
	var count int64
	for _, row := range rows {
		count += row.MessageCount
	}
	assert.Equal(t, int64(4), count)
}
