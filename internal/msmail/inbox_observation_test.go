package msmail

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// nativeSyncGraph decorates actual fakeGraph HTTP responses with the metadata
// a selected Graph message carries. Missing fields are deliberate observations,
// not zero-value defaults. The underlying server still owns MIME and delta state.
type nativeSyncGraph struct {
	*fakeGraph

	metadata   map[string]map[string]any
	omitDelta  map[string][]string
	omitLookup map[string][]string
	lookups    atomic.Int32
}

func newNativeSyncGraph(t *testing.T) *nativeSyncGraph {
	t.Helper()
	f := newFakeGraph(t)
	n := &nativeSyncGraph{fakeGraph: f, metadata: map[string]map[string]any{}, omitDelta: map[string][]string{}, omitLookup: map[string][]string{}}
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		f.serve(recorder, r)
		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		var response map[string]any
		if !assert.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response)) {
			return
		}
		decorate := func(item map[string]any, id string, omitted []string) {
			maps.Copy(item, n.metadata[id])
			if n.metadata[id] != nil && n.metadata[id]["parentFolderId"] == nil {
				item["parentFolderId"] = f.folder[id]
			}
			for _, key := range omitted {
				delete(item, key)
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			if values, ok := response["value"].([]any); ok {
				for _, value := range values {
					item, ok := value.(map[string]any)
					if !assert.True(t, ok, "Graph delta fixture entries must be objects") {
						return
					}
					if item["@removed"] != nil {
						continue
					}
					id, _ := item["id"].(string)
					decorate(item, id, n.omitDelta[id])
				}
			}
		case strings.HasPrefix(r.URL.Path, "/me/messages/") && !strings.HasSuffix(r.URL.Path, "/$value"):
			n.lookups.Add(1)
			for _, field := range []string{"id", "categories", "isRead", "parentFolderId"} {
				assert.Contains(t, strings.Split(r.URL.Query().Get("$select"), ","), field)
			}
			id := strings.TrimPrefix(r.URL.Path, "/me/messages/")
			decorate(response, id, n.omitLookup[id])
		}
		w.WriteHeader(recorder.Code)
		assert.NoError(t, json.MarshalWrite(w, response))
	})
	return n
}

func (n *nativeSyncGraph) complete(categories []string, read bool, revision string) {
	n.metadata["m1"] = map[string]any{"id": "m1", "categories": categories, "isRead": read, "@odata.etag": revision}
}

func nativeImportedState(t *testing.T, st *store.Store, sourceID int64) inboxcontrol.State {
	t.Helper()
	const id = "m1"
	ids, err := st.MessageExistsBatch(sourceID, []string{id})
	require.NoError(t, err)
	require.NotZero(t, ids[id])
	target := inboxcontrol.Target{SourceID: sourceID, SourceType: SourceType, SourceIdentifier: "me@example.com", AccountID: "me@example.com", Scope: inboxcontrol.ScopeMessage, ItemID: ids[id], ProviderID: id}
	state, err := st.GetInboxProviderState(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, state, "sync must persist native provider metadata, including unknown observations")
	return *state
}

func TestMicrosoftInboxSyncCompleteAndSparseObservations(t *testing.T) {
	for _, sparse := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete delta", true: "sparse delta fresh lookup"}[sparse], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newNativeSyncGraph(t)
			f.put("m1", "inbox")
			f.complete([]string{}, false, "version-1")
			if sparse {
				f.omitDelta["m1"] = []string{"isRead", "@odata.etag"}
			}
			st := testutil.NewTestStore(t)
			sum, err := f.sync(t, st)
			requirements.NoError(err)
			before := nativeImportedState(t, st, sum.SourceID)
			requirements.NotNil(before.Read)
			assertions.False(*before.Read)
			requirements.NotNil(before.Inbox)
			assertions.True(*before.Inbox)
			assertions.Equal("inbox", before.Location)
			assertions.Equal("version-1", before.Revision)
			assertions.Empty(before.Tags)
			if sparse {
				assertions.Positive(f.lookups.Load())
			} else {
				assertions.Zero(f.lookups.Load())
			}
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=TRUE WHERE id=?`), before.Target.ItemID)
			requirements.NoError(err)
			f.complete([]string{"Next"}, false, "version-2")
			f.put("m1", "archive")
			_, err = f.sync(t, st)
			requirements.NoError(err)
			after := nativeImportedState(t, st, sum.SourceID)
			assertions.Equal(before.Target, after.Target)
			assertions.Equal("archive", after.Location)
			requirements.NotNil(after.Inbox)
			assertions.False(*after.Inbox)
			assertions.Equal([]string{"Next"}, after.Tags)
			var localRead bool
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), before.Target.ItemID).Scan(&localRead))
			assertions.True(localRead, "sync does not overwrite the archive's UI read state")
		})
	}
}

func TestMicrosoftInboxSyncPartialInvalidatesActionableState(t *testing.T) {
	for _, field := range []string{"categories", "isRead", "parentFolderId", "@odata.etag", "id"} {
		t.Run(field, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newNativeSyncGraph(t)
			f.put("m1", "inbox")
			f.complete([]string{"Old"}, false, "version-1")
			st := testutil.NewTestStore(t)
			sum, err := f.sync(t, st)
			requirements.NoError(err)
			before := nativeImportedState(t, st, sum.SourceID)
			requirements.NotNil(before.Read)
			// A partial delta and a partial independent lookup cannot be combined into
			// an invented full snapshot, even when the previous observation was complete.
			f.complete([]string{"Next"}, true, "version-2")
			f.omitDelta["m1"] = []string{field}
			f.omitLookup["m1"] = []string{field}
			if field == "id" {
				f.omitDelta["m1"] = []string{"isRead"}
			}
			f.put("m1", "inbox")
			_, err = f.sync(t, st)
			requirements.NoError(err)
			after := nativeImportedState(t, st, sum.SourceID)
			assertions.Nil(after.Read)
			assertions.Nil(after.Inbox)
			assertions.Nil(after.Tags)
			assertions.Empty(after.Location)
			assertions.Empty(after.Revision)
			assertions.True(after.ObservedAt.After(before.ObservedAt))
			wantCategories := []string{"Inbox", "Category: Next"}
			if field == "categories" {
				wantCategories = []string{"Inbox", "Category: Old"}
			}
			assertions.ElementsMatch(wantCategories, nativeMessageLabels(t, st, after.Target.ItemID))
		})
	}
}

func TestMicrosoftInboxSyncActualTrashParentPreservesDeletionMark(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newNativeSyncGraph(t)
	f.folders = append(f.folders, "deleteditems")
	f.put("m1", "inbox")
	f.complete([]string{}, false, "version-1")
	st := testutil.NewTestStore(t)
	sum, err := f.sync(t, st)
	requirements.NoError(err)
	before := nativeImportedState(t, st, sum.SourceID)
	requirements.NoError(st.MarkMessagesDeletedBatch(sum.SourceID, []string{"m1"}))
	f.complete([]string{}, false, "version-2")
	// A message can move between the folder delta snapshot and its metadata
	// observation. Its actual parent is authoritative for both state and labels.
	f.metadata["m1"]["parentFolderId"] = "deleteditems"
	f.put("m1", "inbox")
	_, err = f.sync(t, st)
	requirements.NoError(err)
	after := nativeImportedState(t, st, sum.SourceID)
	assertions.Equal("deleteditems", after.Location)
	assertions.ElementsMatch([]string{"Deleteditems"}, nativeMessageLabels(t, st, before.Target.ItemID))
	var deleted bool
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?`), before.Target.ItemID).Scan(&deleted))
	assertions.True(deleted, "native observation must retain the existing staged-deletion fence in Deleted Items")
}

func TestMicrosoftInboxSyncRetryPersistsCompleteMetadata(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newNativeSyncGraph(t)
	f.put("m1", "inbox")
	f.complete([]string{}, false, "version-1")
	f.badValue["m1"] = true
	st := testutil.NewTestStore(t)
	sum, err := f.sync(t, st)
	requirements.NoError(err)
	assertions.Equal(1, sum.Errors)
	delete(f.badValue, "m1")
	_, err = f.sync(t, st)
	requirements.NoError(err)
	after := nativeImportedState(t, st, sum.SourceID)
	requirements.NotNil(after.Read)
	assertions.False(*after.Read)
	assertions.Equal("version-1", after.Revision)
	assertions.Equal("inbox", after.Location)
}

func nativeMessageLabels(t *testing.T, st *store.Store, messageID int64) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`SELECT l.name FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=?`), messageID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var labels []string
	for rows.Next() {
		var label string
		require.NoError(t, rows.Scan(&label))
		labels = append(labels, label)
	}
	require.NoError(t, rows.Err())
	return labels
}

func nativeSourceRevision(t *testing.T, st *store.Store, sourceID int64) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT revision FROM inbox_source_revisions WHERE source_id=?`), sourceID).Scan(&revision))
	return revision
}

func TestMicrosoftInboxSyncKnownEmptyAndOneRevision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newNativeSyncGraph(t)
	f.put("m1", "inbox")
	f.complete([]string{"Old"}, false, "version-1")
	st := testutil.NewTestStore(t)
	sum, err := f.sync(t, st)
	requirements.NoError(err)
	before := nativeImportedState(t, st, sum.SourceID)
	revision := nativeSourceRevision(t, st, sum.SourceID)
	f.complete([]string{}, false, "version-2")
	f.put("m1", "inbox")
	_, err = f.sync(t, st)
	requirements.NoError(err)
	after := nativeImportedState(t, st, sum.SourceID)
	assertions.Empty(after.Tags)
	assertions.Equal("version-2", after.Revision)
	assertions.ElementsMatch([]string{"Inbox"}, nativeMessageLabels(t, st, before.Target.ItemID))
	assertions.Equal(revision+1, nativeSourceRevision(t, st, sum.SourceID), "MIME refresh must not persist the same observation twice")
}

func TestMicrosoftInboxSyncUnlistedParentInvalidatesUntilMapped(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newNativeSyncGraph(t)
	f.put("m1", "inbox")
	f.complete([]string{"Old"}, false, "version-1")
	st := testutil.NewTestStore(t)
	sum, err := f.sync(t, st)
	requirements.NoError(err)
	f.complete([]string{"Next"}, true, "version-2")
	f.put("m1", "unlisted")
	_, err = f.sync(t, st)
	requirements.NoError(err)
	unknown := nativeImportedState(t, st, sum.SourceID)
	assertions.Nil(unknown.Read)
	assertions.Nil(unknown.Inbox)
	assertions.Empty(unknown.Location)
	assertions.Empty(unknown.Revision)
	assertions.ElementsMatch([]string{"Inbox", "Category: Next"}, nativeMessageLabels(t, st, unknown.Target.ItemID))
	f.folders = append(f.folders, "unlisted")
	_, err = f.sync(t, st)
	requirements.NoError(err)
	after := nativeImportedState(t, st, sum.SourceID)
	requirements.NotNil(after.Read)
	assertions.True(*after.Read)
	requirements.NotNil(after.Inbox)
	assertions.False(*after.Inbox)
	assertions.Equal("unlisted", after.Location)
	assertions.ElementsMatch([]string{"Unlisted", "Category: Next"}, nativeMessageLabels(t, st, after.Target.ItemID))
}
