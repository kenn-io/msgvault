package fbmessenger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func dropMessengerAbort(t *testing.T, st *store.Store, table string) {
	t.Helper()
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_messenger_ready_abort ON ` + table)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_messenger_ready_abort`)
	}
	Require.NoError(t, err)
}

func TestImportMessengerMissingCheckpointStillFreezesRecoveredChoice(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticMessengerImports(t, st, true)
	opts := syntheticMessengerImport(t)
	messengerSnapshotAbort(t, st, "message_bodies")
	first, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.True(first.HardErrors)
	var checkpoints int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs WHERE cursor_before IS NOT NULL AND cursor_before<>''`).Scan(&checkpoints))
	assert.Zero(checkpoints)
	opts.NotifyImports = true
	_, err = ImportDYI(t.Context(), st, opts)
	require.ErrorContains(err, "invalid_import_run")
	var runs, messages, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs`).Scan(&runs))
	assert.Equal(1, runs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Zero(messages)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
	dropMessengerAbort(t, st, "message_bodies")
	opts.NotifyImports = false
	second, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(second.HardErrors)
	assert.True(second.WasResumed)
	assert.Equal(first.ImportRunID, second.ImportRunID)
}

func TestImportMessengerJournalFailureRollsBackReadyProjectionAndPolicy(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticMessengerImports(t, st, true)
	opts := syntheticMessengerImport(t)
	opts.NotifyImports = true
	syntheticMessengerMedia(t, opts)
	messengerSnapshotAbort(t, st, "mcp_event_log")
	first, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.True(first.HardErrors)
	for _, table := range []string{"participants", "conversations", "messages", "message_bodies", "message_raw", "attachments", "reactions", "import_message_decisions", "mcp_event_log"} {
		var n int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		assert.Zero(n, table)
	}
	dropMessengerAbort(t, st, "mcp_event_log")
	second, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(second.HardErrors)
	assert.Equal(first.ImportRunID, second.ImportRunID)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

func TestImportMessengerOmittedNativeAggregatesRemainAdditive(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticMessengerImports(t, st, true)
	opts := syntheticMessengerImport(t)
	opts.NotifyImports = true
	first, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(first.HardErrors)
	file := filepath.Join(opts.RootDir, "your_activity_across_facebook/messages/inbox/synthetic_THREAD/message_1.json")
	raw, err := os.ReadFile(file)
	require.NoError(err)
	var thread map[string]any
	require.NoError(json.Unmarshal(raw, &thread))
	messages, ok := thread["messages"].([]any)
	require.True(ok)
	message, ok := messages[0].(map[string]any)
	require.True(ok)
	delete(message, "reactions")
	thread["participants"] = []map[string]any{{"name": "Synthetic Owner"}}
	raw, err = json.Marshal(thread)
	require.NoError(err)
	require.NoError(os.WriteFile(file, raw, 0600))
	second, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(second.HardErrors)
	var reactions, members, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assert.Equal(1, reactions)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM conversation_participants`).Scan(&members))
	assert.Equal(2, members)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}
