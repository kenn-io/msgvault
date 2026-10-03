package msmail

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncMicrosoftCategoriesAndFolderMoves(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGraph(t)
	f.categories = map[string][]string{"m1": {"Inbox", "Next"}}
	f.put("m1", "inbox")
	st := testutil.NewTestStore(t)
	_, err := f.sync(t, st)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"m1"})
	require.NoError(err)
	msg, err := st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Inbox", "Category: Inbox", "Category: Next"}, msg.Labels)
	// A folder update that omits categories must keep the last snapshot.
	f.mu.Lock()
	delete(f.categories, "m1")
	f.mu.Unlock()
	f.put("m1", "archive")
	moved, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, moved.Moved)
	msg, err = st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Archive", "Category: Inbox", "Category: Next"}, msg.Labels)
	// An explicitly empty category collection clears categories, not folders.
	f.mu.Lock()
	f.categories["m1"] = []string{}
	f.mu.Unlock()
	f.put("m1", "archive")
	updated, err := f.sync(t, st)
	require.NoError(err)
	assert.Zero(updated.Moved, "category changes do not move messages")
	msg, err = st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.Equal([]string{"Archive"}, msg.Labels)
}

func TestSyncMicrosoftFolderNameCollisionPreservesCategory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGraph(t)
	f.categories = map[string][]string{"m1": {"Next"}}
	f.put("m1", "inbox")
	st := testutil.NewTestStore(t)
	_, err := f.sync(t, st)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"m1"})
	require.NoError(err)
	f.mu.Lock()
	f.names = map[string]string{"archive": "Category: Next"}
	f.mu.Unlock()
	_, err = f.sync(t, st)
	require.Error(err, "a folder must not take over a category's label")
	msg, err := st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Inbox", "Category: Next"}, msg.Labels)
	var providerID string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_label_id FROM labels WHERE source_id=? AND name=?`), source.ID, "Category: Next").Scan(&providerID))
	assert.Equal("msmail-category:Next", providerID)
}

func TestSyncMicrosoftCategoriesUpgradeExistingCursors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	st := testutil.NewTestStore(t)
	_, err := f.sync(t, st)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"m1"})
	require.NoError(err)
	// These valid provider cursors came from a completed sync that selected
	// only receipt times. No new message event will make the category visible.
	oldCursors := map[string]string{
		"inbox":   f.srv.URL + "/me/mailFolders/inbox/messages/delta?t&token=1",
		"archive": f.srv.URL + "/me/mailFolders/archive/messages/delta?t&token=1",
	}
	blob, err := json.Marshal(oldCursors)
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, SourceType)
	require.NoError(err)
	require.NoError(st.ScopedToSync(source.ID, syncID).CompleteSyncAndPreserveSourceCursorContext(t.Context(), syncID, source.ID, string(blob)))
	f.mu.Lock()
	f.categories = map[string][]string{"m1": {"Next"}}
	f.mu.Unlock()
	f.walkStarts.Store(0)
	f.mimeCalls.Store(0)
	_, err = f.sync(t, st)
	require.NoError(err)
	msg, err := st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Inbox", "Category: Next"}, msg.Labels)
	assert.EqualValues(2, f.walkStarts.Load(), "existing folders refresh their selected metadata")
	assert.Zero(f.mimeCalls.Load(), "the upgrade does not redownload known message bodies")
	f.walkStarts.Store(0)
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Zero(f.walkStarts.Load(), "the next sync resumes the new provider cursors")
}
