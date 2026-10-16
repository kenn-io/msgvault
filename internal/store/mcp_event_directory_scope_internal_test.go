package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sync-scoped writes leave Directory refreshes for later readers. An Events
// ingest view must keep the policy of the Store it was derived from, so live
// capture neither adds refresh work to sync batches nor drops it from root
// writes.
func TestIngestViewKeepsDirectoryRefreshPolicyOfItsStore(t *testing.T) {
	must := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "directory-scope.db"))
	must.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	must.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "directory-scope@example.com")
	must.NoError(err)
	runID, err := st.StartSync(source.ID, "incremental")
	must.NoError(err)
	scoped := st.ScopedToSync(source.ID, runID)
	live := IngestContext{Mode: IngestLive}

	tests := []struct {
		name      string
		view      *Store
		wantDirty bool
	}{
		{name: "sync scoped", view: scoped, wantDirty: true},
		{name: "ingest view of sync scope", view: scoped.WithIngestContext(live), wantDirty: true},
		{name: "ingest view of root", view: st.WithIngestContext(live), wantDirty: false},
		{name: "root", view: st, wantDirty: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			_, err := st.DB().ExecContext(t.Context(), `DELETE FROM directory_projection_dirty`)
			require.NoError(err)
			err = tc.view.withTxContext(t.Context(), func(tx *loggedTx) error {
				_, err := tx.ExecContext(t.Context(), `INSERT INTO directory_projection_dirty(person_id) VALUES (9001)`)
				return err
			})
			require.NoError(err)
			dirty, err := directoryProjectionDirty(t.Context(), st.DB())
			require.NoError(err)
			assert.Equal(t, tc.wantDirty, dirty)
		})
	}
}
