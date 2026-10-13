package imazingcsv

import (
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// Physical deletion must not make the surviving duplicate occurrence look
// new or reuse an allocation whose tombstone still prevents notification replay.
func TestImportImazingDeletedOccurrenceDoesNotRenumberSurvivor(t *testing.T) {
	for _, deletion := range []string{"batch", "all"} {
		t.Run(deletion, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			plain := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic duplicate", "", ""}
			read := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "Synthetic duplicate", "", ""}
			export := newTestExport(t, [][]string{plain, read})
			importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
			first, err := importer.ImportPath(t.Context(), export)
			require.NoError(err)
			require.Equal(2, first.Messages)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
			require.NoError(err)
			evidence := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(evidence, 2)
			removed := findArchivedEvidence(t, evidence, "", "")
			retained := findArchivedEvidence(t, evidence, "Read", "")
			_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?"), time.Now().UTC(), "synthetic-imazing-delete", removed.messageID)
			require.NoError(err)
			var count int64
			if deletion == "batch" {
				count, err = st.DeleteDedupedBatchContext(t.Context(), "synthetic-imazing-delete")
			} else {
				count, _, err = st.DeleteAllDedupedContext(t.Context())
			}
			require.NoError(err)
			require.Equal(int64(1), count)
			writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{read})
			_, err = importer.ImportPath(t.Context(), export)
			require.NoError(err)
			after := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(after, 1, "the surviving native occurrence must not be duplicated after a gap at allocation zero")
			assert.Equal(retained.sourceMessageID, after[0].sourceMessageID)
			assert.Equal(retained.messageID, after[0].messageID)
			// Restoring the deleted export row reuses its original occurrence rather
			// than allocating a new identity or stealing the surviving row's evidence.
			writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{read, plain})
			_, err = importer.ImportPath(t.Context(), export)
			require.NoError(err)
			restored := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(restored, 2)
			assert.Equal(removed.sourceMessageID, findArchivedEvidence(t, restored, "", "").sourceMessageID)
			assert.Equal(retained.messageID, findArchivedEvidence(t, restored, "Read", "").messageID)
		})
	}
}
