package synctechsms

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// Actual routine polling stores the native raw format without import policy or
// decisions. Capture/deletion must adopt that evidence before it is lost.
func TestImportSynctechAdoptsLegacyPollingMMSBeforeIdentityLoss(t *testing.T) {
	for _, boundary := range []string{"capture", "delete-before-capture"} {
		t.Run(boundary, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(SourceType, "+15550000001")
			require.NoError(err)
			syncID, err := st.StartSync(source.ID, AdapterName)
			require.NoError(err)
			path := syntheticSynctechMMSFile(t)
			opts := ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true}
			first, err := NewImporter(st.ScopedToSync(source.ID, syncID), opts).ImportPathIntoSource(source.ID, path)
			require.NoError(err)
			require.Len(first.MessageIDs, 1)
			require.NoError(st.CompleteSync(syncID, ""))
			var decisions int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
			assert.Zero(decisions)
			var remoteID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM participant_identifiers WHERE identifier_type=? AND identifier_value=?`), ParticipantIdentifierType, "+15550000002").Scan(&remoteID))
			survivor, err := st.EnsureParticipantByPhone("+15550000003", "Synthetic Survivor", ParticipantIdentifierType)
			require.NoError(err)
			require.NoError(st.MergeParticipants(remoteID, survivor))
			if boundary == "delete-before-capture" {
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?`), time.Now().UTC(), "synthetic-synctech-legacy-delete", first.MessageIDs[0])
				require.NoError(err)
				removed, err := st.DeleteDedupedBatchContext(t.Context(), "synthetic-synctech-legacy-delete")
				require.NoError(err)
				assert.Equal(int64(1), removed)
			}
			configureSyntheticSynctechImport(t, st, true)
			opts.NotifyImports = true
			replay, err := NewImporter(st, opts).ImportPath(path)
			require.NoError(err)
			assert.Equal(1, replay.MMSImported)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			assert.Zero(events, "legacy raw identity must be adopted before native-ID-dependent replay")
		})
	}
}
