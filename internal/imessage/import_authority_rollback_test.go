package imessage

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportImessageJournalFailureRollsBackNativeProjectionAndCanRetry(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticAppleImports(t, st, true)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imessage_journal_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required journal failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_journal_abort BEFORE INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION synthetic_imessage_journal_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_journal_abort BEFORE INSERT ON mcp_event_log BEGIN SELECT RAISE(ABORT,'synthetic required journal failure'); END`)
	}
	require.NoError(err)
	client, err := syntheticAppleImportClient(t, WithNotifyImports(true))
	require.NoError(err)
	first, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	assert.Zero(first.MessagesImported)
	assert.Equal(1, first.Skipped)
	for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "message_labels", "import_message_decisions", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count))
		assert.Zero(count, "journal failure leaked "+name)
	}
	query := `DROP TRIGGER synthetic_imessage_journal_abort`
	if st.IsPostgreSQL() {
		query += ` ON mcp_event_log`
	}
	_, err = st.DB().Exec(query)
	require.NoError(err)
	retry, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	assert.Equal(1, retry.MessagesImported)
	assert.NotEqual(first.ImportRunID, retry.ImportRunID)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

func TestImportImessageDoesNotInheritAnotherNativeImportRun(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-apple", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "apple_messages", Kinds: []string{"message"}}, {Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
	require.NoError(err)
	foreign, err := st.GetOrCreateSource("eml", "foreign@example.test")
	require.NoError(err)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	sync, err := st.StartSync(foreign.ID, "synthetic-foreign-import")
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, st.FailSync(sync, "synthetic cleanup")) })
	view := st.ScopedToSync(foreign.ID, sync)
	run, err := view.BeginNativeImportAttempt(t.Context(), foreign.ID, "eml", true, sync, 0)
	require.NoError(err)
	view, err = view.WithImportRun(t.Context(), run.ID)
	require.NoError(err)
	client, err := syntheticAppleImportClient(t)
	require.NoError(err)
	summary, err := client.Import(t.Context(), view, source.ID)
	require.NoError(err)
	require.Equal(1, summary.MessagesImported)
	assert.NotEqual(run.ID, summary.ImportRunID)
	var format, status string
	var notify bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT import_format,notify_imports FROM import_runs WHERE id=?`), summary.ImportRunID).Scan(&format, &notify))
	assert.Equal("apple_messages", format)
	assert.False(notify)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT status FROM sync_runs WHERE id=?`), sync).Scan(&status))
	assert.Equal("running", status, "the offline call does not finalize its caller's operation")
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}
