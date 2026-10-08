package slack

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectedChannelIDsConstrainHistoryAndReplySearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := testWorkspace(t)
	f.convs[0].Name = "renamed"
	var queries []string
	f.onHistory = func(channel string) { assert.Equal("C01", channel) }
	f.onSearch = func(query string, _ int) {
		queries = append(queries, query)
		assert.Contains(query, "in:<#C01>")
	}
	imp, opts := testImporter(t, f)
	opts.ChannelIDs = []string{"C01"}
	opts.ExcludePrivateChannels, opts.ExcludeDMs, opts.ExcludeGroupDMs = true, true, true
	imp.now = func() time.Time { return tsBase.Add(24 * time.Hour) }
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, summary.ConversationsProcessed)
	require.NotEmpty(queries)
	root := &f.convs[0].Msgs[1]
	root.Replies = append(root.Replies, fakeMsg{TS: ts(1500), ThreadTS: root.TS, User: "UBOB", Text: "late selected reply"})
	imp.now = func() time.Time { return tsBase.Add(26 * time.Hour) }
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
	assert.Equal(11, count, "selected history plus replies, without duplicate roots")
	opts.ChannelIDs = []string{}
	summary, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Zero(summary.ConversationsProcessed, "an explicit empty selection collects nothing")
}
