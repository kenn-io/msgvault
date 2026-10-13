package imazingcsv

import (
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A modeled unallocated native archive exercises the cutover consumers; this
// does not replace the separate actual old-version database upgrade fixture.
func TestImportImazingLegacyAllocationAdoptedBeforeCaptureOrDeletion(t *testing.T) {
	for _, boundary := range []string{"capture", "batch", "all"} {
		t.Run(boundary, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			plain := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic legacy body", "", ""}
			read := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "Synthetic legacy body", "", ""}
			delivered := []string{"Synthetic Contact", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "Synthetic legacy body", "", ""}
			export := newTestExport(t, [][]string{plain, read, delivered})
			importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
			_, err := importer.ImportPath(t.Context(), export)
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
			require.NoError(err)
			before := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(before, 3)
			removed := findArchivedEvidence(t, before, "", "")
			retained := findArchivedEvidence(t, before, "Read", "")
			_, err = st.DB().Exec("DELETE FROM import_occurrence_allocations")
			require.NoError(err)
			if boundary == "capture" {
				_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-legacy", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
				require.NoError(err)
				var adopted int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&adopted))
				assert.Equal(3, adopted, "capture cutover adopts the complete native source group")
			}
			_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?"), time.Now().UTC(), "synthetic-legacy-delete", removed.messageID)
			require.NoError(err)
			var deleted int64
			if boundary == "all" {
				deleted, _, err = st.DeleteAllDedupedContext(t.Context())
			} else {
				deleted, err = st.DeleteDedupedBatchContext(t.Context(), "synthetic-legacy-delete")
			}
			require.NoError(err)
			require.Equal(int64(1), deleted)
			var adopted int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&adopted))
			assert.Equal(3, adopted, "physical identity loss must retain all duplicate allocation evidence")
			writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{read})
			_, err = importer.ImportPath(t.Context(), export)
			require.NoError(err)
			after := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(after, 2, "reimport must not duplicate the surviving occurrence after allocation zero is deleted")
			assert.Equal(retained.messageID, findArchivedEvidence(t, after, "Read", "").messageID)
			writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{delivered, read, plain})
			_, err = importer.ImportPath(t.Context(), export)
			require.NoError(err)
			restored := archivedDuplicateEvidence(t, st, source.ID)
			require.Len(restored, 3)
			assert.Equal(removed.sourceMessageID, findArchivedEvidence(t, restored, "", "").sourceMessageID)
			var events int
			require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
			assert.Zero(events)
		})
	}
}

func TestImportImazingLegacyAllocationFailurePreservesArchive(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	row := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic retained body", "", ""}
	export := newTestExport(t, [][]string{row})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	_, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	_, err = st.DB().Exec("DELETE FROM import_occurrence_allocations")
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_legacy_allocation_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic allocation adoption failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_legacy_allocation_abort BEFORE INSERT ON import_occurrence_allocations FOR EACH ROW EXECUTE FUNCTION synthetic_legacy_allocation_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_legacy_allocation_abort BEFORE INSERT ON import_occurrence_allocations BEGIN SELECT RAISE(ABORT,'synthetic allocation adoption failure'); END`)
	}
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET deleted_at=?,delete_batch_id=?"), time.Now().UTC(), "synthetic-adoption-failure")
	require.NoError(err)
	_, err = st.DeleteDedupedBatchContext(t.Context(), "synthetic-adoption-failure")
	require.ErrorContains(err, "synthetic allocation adoption failure")
	var messages int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
	assert.Equal(1, messages, "failed adoption must roll back physical deletion")
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_legacy_allocation_abort ON import_occurrence_allocations`)
		require.NoError(err)
		_, err = st.DB().Exec(`DROP FUNCTION synthetic_legacy_allocation_abort()`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_legacy_allocation_abort`)
	}
	require.NoError(err)
	deleted, err := st.DeleteDedupedBatchContext(t.Context(), "synthetic-adoption-failure")
	require.NoError(err)
	assert.Equal(int64(1), deleted)
	var allocations int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&allocations))
	assert.Equal(1, allocations)
}
