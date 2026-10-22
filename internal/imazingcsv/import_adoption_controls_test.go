package imazingcsv

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingDisabledCaptureDoesNotRequireLegacyRawRecovery(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	row := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic recovery body", "", ""}
	export := newTestExport(t, [][]string{row})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	_, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	_, err = st.DB().Exec("DELETE FROM import_occurrence_allocations")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE message_raw SET raw_data=?,compression=''"), []byte("{"))
	require.NoError(err)
	_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{})
	require.NoError(err, "disabled Events must preserve existing silent archive recovery")
	_, err = importer.ImportPath(t.Context(), export)
	require.NoError(err, "the native silent importer can restore the actual raw projection")
	var count int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&count))
	assert.Equal(1, count)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&count))
	assert.Equal(1, count)
}

func TestImportImazingAllocationTracksNativeCanonicalAliases(t *testing.T) {
	for _, mode := range []string{"rewrite", "merge"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			plain := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic alias body", "", ""}
			read := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "Synthetic alias body", "", ""}
			export := newTestExport(t, [][]string{plain, read})
			importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
			_, err := importer.ImportPath(t.Context(), export)
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
			require.NoError(err)
			before := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(before, 2)
			plainRecord := findArchivedEvidence(t, before, "", "")
			readRecord := findArchivedEvidence(t, before, "Read", "")
			alias := "imazing_csv:" + stableHash("synthetic-current-canonical-alias")
			want := 2
			if mode == "merge" {
				alias = readRecord.sourceMessageID
				want = 1
			}
			var conversationID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), plainRecord.messageID).Scan(&conversationID))
			require.NoError(st.MigrateSourceMessageID(source.ID, conversationID, plainRecord.sourceMessageID, alias))
			_, err = importer.ImportPath(t.Context(), export)
			require.NoError(err)
			keys := sourceMessageIDs(t, st, source.ID)
			assert.Len(keys, want)
			assert.Contains(keys, alias)
			assert.NotContains(keys, plainRecord.sourceMessageID, "reimport must follow the native canonical alias instead of recreating its old key")
			var allocations int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&allocations))
			assert.Equal(2, allocations, "both logical occurrence allocations survive the native merge")
		})
	}
}
