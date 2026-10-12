package imazingcsv

import (
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// This models unallocated archive rows, not an old-version migration proof.
// Adopting one row from a contracted export must not hide other retained rows
// from the next native duplicate reconciliation.
func TestImportImazingPartialAllocationAdoptionKeepsArchivedOccurrences(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	plain := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic adoption body", "", ""}
	read := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "2024-06-01 12:00:10", "iMessage", "Outgoing", "", "", "Read", "", "", "Synthetic adoption body", "", ""}
	delivered := []string{"Synthetic Contact", "2024-06-01 12:00:00", "2024-06-01 12:00:05", "", "iMessage", "Outgoing", "", "", "Delivered", "", "", "Synthetic adoption body", "", ""}
	export := newTestExport(t, [][]string{plain, read, delivered})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	_, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	before := sourceMessageIDs(t, st, source.ID)
	require.Len(before, 3)
	// Keep the native archive projection and remove only the new allocation
	// records to exercise an archive without this additive evidence.
	_, err = st.DB().Exec("DELETE FROM import_occurrence_allocations")
	require.NoError(err)
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{read})
	_, err = importer.ImportPath(t.Context(), export)
	require.NoError(err)
	assert.ElementsMatch(before, sourceMessageIDs(t, st, source.ID))
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{read, plain})
	_, err = importer.ImportPath(t.Context(), export)
	require.NoError(err)
	assert.ElementsMatch(before, sourceMessageIDs(t, st, source.ID), "partial allocation adoption must also consider unallocated archived occurrences")
}
