package imessage

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportImessageFreshLogicalRunsDoNotInheritLiveMode(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message(date,text) VALUES(757425600,'Synthetic logical Apple message')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	client, err := NewClient(path)
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, client.Close()) })
	for range 2 {
		summary, err := client.Import(t.Context(), st.WithIngestContext(store.IngestContext{Mode: store.IngestLive}), source.ID)
		require.NoError(err)
		require.Equal(1, summary.MessagesImported)
	}
	var runs, completed, syncs, messages, decisions, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs`).Scan(&runs))
	assert.Equal(2, runs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE AND import_format='apple_messages' AND notify_imports=FALSE`).Scan(&completed))
	assert.Equal(2, completed)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs WHERE status='completed'`).Scan(&syncs))
	assert.Equal(2, syncs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
	assert.Equal(1, decisions)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}
