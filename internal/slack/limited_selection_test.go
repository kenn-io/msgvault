package slack

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimitedSelectedChannelsRotateSweepsAcrossRestarts(t *testing.T) {
	require := require.New(t)
	f := testWorkspace(t)
	now := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
	rootTS := tsFormat(now.Add(-48 * time.Hour))
	f.convs = []*fakeConv{
		{ID: "C01", Name: "first", Kind: "public", Members: []string{"UME"}, Msgs: []fakeMsg{{TS: rootTS, User: "UME", Text: "First root"}}},
		{ID: "C02", Name: "second", Kind: "public", Members: []string{"UME"}, Msgs: []fakeMsg{{TS: rootTS, User: "UME", Text: "Second root"}}},
	}
	imp, opts := testImporter(t, f)
	imp.now = func() time.Time { return now }
	opts.ChannelIDs = []string{"C01", "C02"}
	// Finish each initial canonical audit so only search can discover the new
	// replies during these runs; periodic audits are not due again for a week.
	for range 3 {
		_, err := imp.Import(t.Context(), opts)
		require.NoError(err)
	}
	lateTS := tsFormat(now.Add(30 * time.Minute))
	f.mu.Lock()
	for _, channel := range f.convs {
		channel.Msgs[0].Replies = []fakeMsg{{TS: lateTS, ThreadTS: rootTS, User: "UME", Text: "Late reply"}}
	}
	f.mu.Unlock()
	now = now.Add(time.Hour)
	opts.Limit = 1
	for range 6 {
		restarted := NewImporter(imp.store, imp.client, "T01")
		restarted.now = func() time.Time { return now }
		_, err := restarted.Import(t.Context(), opts)
		require.NoError(err)
	}
	for _, id := range []string{"C01", "C02"} {
		var count int
		require.NoError(imp.store.DB().QueryRow(imp.store.Rebind("SELECT count(*) FROM messages WHERE source_message_id = ?"), id+":"+lateTS).Scan(&count))
		assert.Equal(t, 1, count, "late reply in %s must be discovered under a standing limit", id)
	}
}

func TestLimitedSelectedDMAdvancesIncrementalHistory(t *testing.T) {
	require := require.New(t)
	f := testWorkspace(t)
	now := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
	f.convs = []*fakeConv{{ID: "D01", Kind: "im", IMUser: "UALICE", Msgs: []fakeMsg{{TS: tsFormat(now.Add(-time.Hour)), User: "UALICE", Text: "Old message"}}}}
	imp, opts := testImporter(t, f)
	imp.now = func() time.Time { return now }
	opts.ChannelIDs = []string{"D01"}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	newTS := tsFormat(now.Add(30 * time.Minute))
	f.mu.Lock()
	f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: newTS, User: "UALICE", Text: "New message"})
	f.mu.Unlock()
	now = now.Add(time.Hour)
	opts.Limit = 1
	for range 8 {
		_, err := imp.Import(t.Context(), opts)
		require.NoError(err)
	}
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind("SELECT count(*) FROM messages WHERE source_message_id = ?"), "D01:"+newTS).Scan(&count))
	assert.Equal(t, 1, count, "canonical thread walks must let incremental history archive new top-level messages")
}

func TestSelectedDMDiscoversLateReplies(t *testing.T) {
	require := require.New(t)
	f := testWorkspace(t)
	now := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
	rootTS := tsFormat(now.Add(-48 * time.Hour))
	f.convs = []*fakeConv{{ID: "D01", Kind: "im", IMUser: "UALICE", Msgs: []fakeMsg{{TS: rootTS, User: "UALICE", Text: "DM root"}}}}
	imp, opts := testImporter(t, f)
	imp.now = func() time.Time { return now }
	opts.ChannelIDs = []string{"D01"}
	for range 3 {
		_, err := imp.Import(t.Context(), opts)
		require.NoError(err)
	}
	// Scoped reply search covers only channels, so selected DMs must audit
	// their own threads to find this reply.
	lateTS := tsFormat(now.Add(30 * time.Minute))
	f.mu.Lock()
	f.convs[0].Msgs[0].Replies = []fakeMsg{{TS: lateTS, ThreadTS: rootTS, User: "UALICE", Text: "Late DM reply"}}
	f.mu.Unlock()
	now = now.Add(time.Hour)
	for range 2 {
		_, err := imp.Import(t.Context(), opts)
		require.NoError(err)
	}
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind("SELECT count(*) FROM messages WHERE source_message_id = ?"), "D01:"+lateTS).Scan(&count))
	assert.Equal(t, 1, count, "a selected DM must discover late thread replies")
}

func TestSelectedChannelMissingFromListingIsReported(t *testing.T) {
	require := require.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	opts.ChannelIDs = []string{"C01", "C99"}
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(t, 1, summary.ConversationsProcessed)
	assert.Equal(t, []string{"C99"}, summary.UnavailableChannels)
	opts.ChannelIDs = []string{"C01"}
	summary, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Empty(t, summary.UnavailableChannels)
}
