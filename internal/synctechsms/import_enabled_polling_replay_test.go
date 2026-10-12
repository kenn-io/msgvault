package synctechsms

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSynctechPollingAfterEventsStartupRetainsSilentMMSIdentity(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticSynctechImport(t, st, true)
	source, err := st.GetOrCreateSource(SourceType, "+15550000001")
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, AdapterName)
	require.NoError(err)
	path := syntheticSynctechMMSFile(t)
	summary, err := NewImporter(st.ScopedToSync(source.ID, syncID), ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true}).ImportPathIntoSource(source.ID, path)
	require.NoError(err)
	assert.Equal(1, summary.MMSImported)
	require.NoError(st.CompleteSync(syncID, ""))
	for _, table := range []string{"import_runs", "import_message_decisions", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		assert.Zero(count, "routine polling cannot create opted-in import policy")
	}
	var remoteID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM participant_identifiers WHERE identifier_type=? AND identifier_value=?`), ParticipantIdentifierType, "+15550000002").Scan(&remoteID))
	survivor, err := st.EnsureParticipantByPhone("+15550000003", "Synthetic Survivor", ParticipantIdentifierType)
	require.NoError(err)
	require.NoError(st.MergeParticipants(remoteID, survivor))
	replay, err := NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true, NotifyImports: true}).ImportPath(path)
	require.NoError(err)
	assert.Equal(1, replay.MMSImported)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Zero(events, "existing content polled after startup cannot become an import notification after a merge")
}
