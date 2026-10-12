package fbmessenger

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportMessengerFreshLogicalRunsDoNotInheritLiveMode(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	opts := syntheticMessengerImport(t)
	for range 2 {
		summary, err := ImportDYI(t.Context(), st.WithIngestContext(store.IngestContext{Mode: store.IngestLive}), opts)
		require.NoError(err)
		require.Equal(int64(1), summary.MessagesAdded)
	}
	var runs, completed, syncs, messages, decisions, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs`).Scan(&runs))
	assert.Equal(2, runs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE AND import_format='facebook_messenger' AND notify_imports=FALSE`).Scan(&completed))
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
