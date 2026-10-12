package whatsapp

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportWhatsAppAndroidFreshLogicalRunsDoNotInheritLiveMode(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := createGroupParticipantsImportFixture(t, nil)
	st := testutil.NewTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Synthetic Owner"
	client := NewImporter(st.WithIngestContext(store.IngestContext{Mode: store.IngestLive}), nil)
	for range 2 {
		summary, err := client.Import(t.Context(), path, opts)
		require.NoError(err)
		require.Equal(int64(2), summary.MessagesAdded)
	}
	var runs, completed, syncs, messages, decisions, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs`).Scan(&runs))
	assert.Equal(2, runs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE AND import_format='whatsapp' AND notify_imports=FALSE`).Scan(&completed))
	assert.Equal(2, completed)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs WHERE status='completed'`).Scan(&syncs))
	assert.Equal(2, syncs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(2, messages)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
	assert.Equal(2, decisions)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}
