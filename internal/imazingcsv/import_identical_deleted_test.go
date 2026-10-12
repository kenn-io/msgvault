package imazingcsv

import (
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingIdenticalDeletedOccurrenceRematchesLiveSurvivor(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	row := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic identical occurrence", "", ""}
	export := newTestExport(t, [][]string{row, row})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	_, err := importer.ImportPath(t.Context(), export)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "+15550000001")
	require.NoError(err)
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind("SELECT id,source_message_id FROM messages WHERE source_id=? ORDER BY id"), source.ID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	type occurrence struct {
		id  int64
		key string
	}
	var before []occurrence
	for rows.Next() {
		var item occurrence
		require.NoError(rows.Scan(&item.id, &item.key))
		before = append(before, item)
	}
	require.NoError(rows.Err())
	require.Len(before, 2)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?"), time.Now().UTC(), "synthetic-identical-delete", before[0].id)
	require.NoError(err)
	deleted, err := st.DeleteDedupedBatchContext(t.Context(), "synthetic-identical-delete")
	require.NoError(err)
	require.Equal(int64(1), deleted)
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{row})
	_, err = importer.ImportPath(t.Context(), export)
	require.NoError(err)
	keys := sourceMessageIDs(t, st, source.ID)
	require.Len(keys, 1, "identical surviving evidence must prefer the archived live occurrence over its deleted twin")
	assert.Equal(before[1].key, keys[0])
	// An expanded identical group reuses its existing tombstone without allocating
	// a third logical occurrence or changing the surviving archive row identity.
	writeTestCSV(t, filepath.Join(export, "csv", "messages.csv"), [][]string{row, row})
	_, err = importer.ImportPath(t.Context(), export)
	require.NoError(err)
	assert.ElementsMatch([]string{before[0].key, before[1].key}, sourceMessageIDs(t, st, source.ID))
	var allocations int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&allocations))
	assert.Equal(2, allocations)
}
