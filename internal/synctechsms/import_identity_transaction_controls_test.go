package synctechsms

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSynctechJournalFailureRollsBackRetainedIdentity(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticSynctechImport(t, st, true)
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_synctech_journal_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic native journal failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_journal_abort BEFORE INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION synthetic_synctech_journal_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_journal_abort BEFORE INSERT ON mcp_event_log BEGIN SELECT RAISE(ABORT,'synthetic native journal failure'); END`)
	}
	require.NoError(err)
	path := syntheticSynctechMMSFile(t)
	importer := NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true, IncludeAttachments: true, AttachmentsDir: t.TempDir(), NotifyImports: true})
	failed, err := importer.ImportPath(path)
	require.Error(err)
	require.NotEmpty(failed.ImportRunID)
	assert.Empty(failed.MessageIDs)
	for _, table := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "attachments", "import_message_decisions", "import_message_identities", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		assert.Zero(count, "journal failure must roll back "+table)
	}
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_synctech_journal_abort ON mcp_event_log`)
		require.NoError(err)
		_, err = st.DB().Exec(`DROP FUNCTION synthetic_synctech_journal_abort()`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_synctech_journal_abort`)
	}
	require.NoError(err)
	completed, err := importer.ImportPath(path)
	require.NoError(err)
	assert.NotEqual(failed.ImportRunID, completed.ImportRunID)
	assert.Equal(1, completed.MMSImported)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events, "rolled-back identity cannot consume a retry's real notification")
}

func TestImportSynctechRetainedIdentityIsBoundToSourceLifetime(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticSynctechImport(t, st, true)
	path := syntheticSynctechMMSFile(t)
	opts := ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true}
	_, err := NewImporter(st, opts).ImportPath(path)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, opts.OwnerPhone)
	require.NoError(err)
	require.NoError(st.RemoveSource(source.ID))
	opts.NotifyImports = true
	fresh, err := NewImporter(st, opts).ImportPath(path)
	require.NoError(err)
	assert.Equal(1, fresh.MMSImported)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events, "a retired source lifetime cannot suppress fresh-source notification")
}
