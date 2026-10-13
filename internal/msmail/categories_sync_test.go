package msmail

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
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
	f.version["m1"] = 1
	f.mu.Unlock()
	f.put("m1", "archive")
	moved, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, moved.Moved)
	msg, err = st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Archive", "Category: Inbox", "Category: Next"}, msg.Labels)
	assert.Equal("body m1 v1", msg.Snippet)
	// Updated categories survive the same download that refreshes the body.
	f.mu.Lock()
	f.categories["m1"] = []string{"Later"}
	f.version["m1"] = 2
	f.mu.Unlock()
	f.put("m1", "archive")
	updated, err := f.sync(t, st)
	require.NoError(err)
	assert.Zero(updated.Moved)
	msg, err = st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Archive", "Category: Later"}, msg.Labels)
	assert.Equal("body m1 v2", msg.Snippet)
	// An explicitly empty category collection clears categories, not folders.
	f.mu.Lock()
	f.categories["m1"] = []string{}
	f.mu.Unlock()
	f.put("m1", "archive")
	updated, err = f.sync(t, st)
	require.NoError(err)
	assert.Zero(updated.Moved, "category changes do not move messages")
	msg, err = st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.Equal([]string{"Archive"}, msg.Labels)
}

func TestSyncMicrosoftFolderNameCollisionKeepsBothLabels(t *testing.T) {
	tests := []struct {
		name        string
		folderFirst bool
	}{
		{name: "category first"},
		{name: "folder first", folderFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeGraph(t)
			f.put("m1", "inbox")
			f.put("m2", "archive")
			st := testutil.NewTestStore(t)
			step := func(categories bool) {
				f.mu.Lock()
				if categories {
					f.categories = map[string][]string{"m1": {"Next"}}
				} else {
					f.names = map[string]string{"archive": "Category: Next"}
				}
				f.mu.Unlock()
				if categories {
					f.put("m1", "inbox")
				}
				_, err := f.sync(t, st)
				require.NoError(err, "a folder and a category with the same display name both sync")
			}
			step(!tt.folderFirst)
			step(tt.folderFirst)
			step(true)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
			require.NoError(err)
			ids, err := st.MessageExistsBatch(source.ID, []string{"m1", "m2"})
			require.NoError(err)
			msg, err := st.GetMessage(ids["m1"])
			require.NoError(err)
			assert.ElementsMatch([]string{"Inbox", "Category: Next (2)"}, msg.Labels)
			msg, err = st.GetMessage(ids["m2"])
			require.NoError(err)
			assert.ElementsMatch([]string{"Category: Next"}, msg.Labels)
			var providerID string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_label_id FROM labels WHERE source_id=? AND name=?`), source.ID, "Category: Next (2)").Scan(&providerID))
			assert.Equal("msmail-category:Next", providerID)
		})
	}
}

func TestSyncMicrosoftCategoriesUpgradeExistingCursors(t *testing.T) {
	for _, tt := range []struct {
		name          string
		pendingUpdate bool
		expired       bool
		expiredWalk   bool
	}{
		{name: "unchanged"},
		{name: "pending update", pendingUpdate: true},
		{name: "expired cursor", expired: true},
		{name: "expired category walk", pendingUpdate: true, expiredWalk: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeGraph(t)
			f.withAttachment["m1"] = true
			f.put("m1", "inbox")
			f.put("m2", "inbox")
			st := testutil.NewTestStore(t)
			_, err := f.sync(t, st)
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
			require.NoError(err)
			ids, err := st.MessageExistsBatch(source.ID, []string{"m1"})
			require.NoError(err)
			// A completed sync from before category support selected only receipt times.
			oldCursors := map[string]string{
				"inbox":   f.srv.URL + "/me/mailFolders/inbox/messages/delta?t&token=2",
				"archive": f.srv.URL + "/me/mailFolders/archive/messages/delta?t&token=2",
			}
			blob, err := json.Marshal(oldCursors)
			require.NoError(err)
			syncID, err := st.StartSync(source.ID, SourceType)
			require.NoError(err)
			require.NoError(st.ScopedToSync(source.ID, syncID).CompleteSyncAndPreserveSourceCursorContext(t.Context(), syncID, source.ID, string(blob)))
			f.mu.Lock()
			f.categories = map[string][]string{"m1": {"Next"}}
			f.expired["inbox"] = tt.expired
			if tt.expiredWalk {
				f.pageSize = 1
				f.stopAt = 1
				f.stopStatus = http.StatusGone
			}
			if tt.pendingUpdate {
				f.withAttachment["m1"] = false
				f.version["m1"] = 1
			}
			f.mu.Unlock()
			if tt.pendingUpdate {
				f.put("m1", "inbox")
			}
			f.walkStarts.Store(0)
			f.mimeCalls.Store(0)
			_, err = f.sync(t, st)
			require.NoError(err)
			msg, err := st.GetMessage(ids["m1"])
			require.NoError(err)
			assert.ElementsMatch([]string{"Inbox", "Category: Next"}, msg.Labels)
			if tt.expiredWalk {
				assert.EqualValues(3, f.walkStarts.Load(), "the interrupted category walk starts over")
			} else {
				assert.EqualValues(2, f.walkStarts.Load(), "existing folders refresh their selected metadata")
			}
			if tt.pendingUpdate {
				assert.Equal("body m1 v1", strings.TrimSpace(msg.BodyText))
				assert.Equal([2]int{0, 0}, attachments(t, st), "attachments removed by the pending update leave the archive")
				assert.EqualValues(1, f.mimeCalls.Load(), "the old cursor's pending update refreshes MIME")
			} else {
				assert.Zero(f.mimeCalls.Load(), "the upgrade does not redownload unchanged message bodies")
			}
			f.walkStarts.Store(0)
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Zero(f.walkStarts.Load(), "the next sync resumes the new provider cursors")
		})
	}
}

func TestSyncMicrosoftCategoriesUpgradeResumes(t *testing.T) {
	for _, tt := range []struct {
		name           string
		pendingUpdates int
		remainingMIME  int
		stopInDelta    bool
	}{
		{name: "interrupted delta", pendingUpdates: 3, remainingMIME: 2, stopInDelta: true},
		{name: "interrupted category walk", pendingUpdates: 1, remainingMIME: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeGraph(t)
			f.folders = []string{"inbox"}
			for _, id := range []string{"m1", "m2", "m3"} {
				f.put(id, "inbox")
			}
			st := testutil.NewTestStore(t)
			initial, err := f.sync(t, st)
			require.NoError(err)
			blob, err := json.Marshal(map[string]string{
				"inbox": f.srv.URL + "/me/mailFolders/inbox/messages/delta?t&token=3",
			})
			require.NoError(err)
			syncID, err := st.StartSync(initial.SourceID, SourceType)
			require.NoError(err)
			require.NoError(st.ScopedToSync(initial.SourceID, syncID).CompleteSyncAndPreserveSourceCursorContext(t.Context(), syncID, initial.SourceID, string(blob)))
			f.mu.Lock()
			f.categories = map[string][]string{"m1": {"Next"}, "m2": {"Next"}, "m3": {"Next"}}
			f.stopAt = 2
			f.stopInDelta = tt.stopInDelta
			for i := 1; i <= tt.pendingUpdates; i++ {
				f.version[fmt.Sprintf("m%d", i)] = 1
			}
			f.mu.Unlock()
			for i := 1; i <= tt.pendingUpdates; i++ {
				f.put(fmt.Sprintf("m%d", i), "inbox")
			}
			_, err = f.sync(t, st)
			require.Error(err)
			f.mu.Lock()
			f.version["m1"] = 2
			f.mu.Unlock()
			f.put("m1", "inbox")

			f.walkStarts.Store(0)
			f.mimeCalls.Store(0)
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.EqualValues(tt.remainingMIME, f.mimeCalls.Load(), "resume refreshes only the undrained changes")
			assert.EqualValues(1, f.walkStarts.Load(), "resume completes a category walk from the start")
			ids, err := st.MessageExistsBatch(initial.SourceID, []string{"m1", "m2", "m3"})
			require.NoError(err)
			require.Len(ids, 3)
			for _, id := range ids {
				msg, err := st.GetMessage(id)
				require.NoError(err)
				assert.ElementsMatch([]string{"Inbox", "Category: Next"}, msg.Labels)
			}
			if tt.pendingUpdates == 3 {
				assert.Equal(map[string]string{"m1": "body m1 v2", "m2": "body m2 v1", "m3": "body m3 v1"}, snippets(t, st))
			} else {
				assert.Equal(map[string]string{"m1": "body m1 v2", "m2": "body m2 v0", "m3": "body m3 v0"}, snippets(t, st))
			}
		})
	}
}

func TestSyncMicrosoftCategoriesUpgradeIncludesUpdatesDuringWalk(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGraph(t)
	f.folders = []string{"inbox"}
	f.put("m1", "inbox")
	st := testutil.NewTestStore(t)
	initial, err := f.sync(t, st)
	require.NoError(err)
	blob, err := json.Marshal(map[string]string{
		"inbox": f.srv.URL + "/me/mailFolders/inbox/messages/delta?t&token=1",
	})
	require.NoError(err)
	syncID, err := st.StartSync(initial.SourceID, SourceType)
	require.NoError(err)
	require.NoError(st.ScopedToSync(initial.SourceID, syncID).CompleteSyncAndPreserveSourceCursorContext(t.Context(), syncID, initial.SourceID, string(blob)))
	f.mu.Lock()
	f.changeOnWalk = "m1"
	f.mu.Unlock()
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "body m1 v1"}, snippets(t, st))
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "body m1 v1"}, snippets(t, st), "the replacement cursor must not skip the content update")
}
