package slack

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Checkpoints written before principal tracking carry no principal. They were
// written by the source's own user, so revalidating that same user must not
// discard every conversation's coverage and re-walk the workspace.
func TestRevalidationKeepsCoverageFromUntrackedOwnerCheckpoint(t *testing.T) {
	require := require.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	opts.ChannelIDs = []string{"C01"}
	opts.NoThreads = true
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)

	var syncID int64
	var cursor string
	require.NoError(imp.store.DB().QueryRow(`SELECT id, cursor_after FROM sync_runs
		WHERE status = 'completed' ORDER BY id DESC LIMIT 1`).Scan(&syncID, &cursor))
	var state map[string]any
	require.NoError(json.Unmarshal([]byte(cursor), &state))
	require.Equal("UME", state["principal_id"])
	delete(state, "principal_id")
	legacy, err := json.Marshal(state)
	require.NoError(err)
	_, err = imp.store.DB().Exec(imp.store.Rebind(`UPDATE sync_runs SET cursor_after = ? WHERE id = ?`), string(legacy), syncID)
	require.NoError(err)

	historyCalls := 0
	f.onHistory = func(string) { historyCalls++ }
	opts.RevalidatePrincipal = true
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(t, 1, historyCalls, "an incremental fetch, not a re-walk of all history pages")
}
