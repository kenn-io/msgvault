package threads

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type importFixture struct {
	st             *store.Store
	sourceID       int64
	server         *httptest.Server
	clock          *testClock
	mu             sync.Mutex
	calls          []string
	posts, replies []Media
	conversations  map[string][]Media
	failure        func(http.ResponseWriter, *http.Request) bool
	pageSize       int
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	w, st := writerFixture(t)
	source, err := st.GetSourceByTypeAndIdentifier("threads", writerSource)
	require.NoError(t, err)
	cfg := SourceSyncConfig{Version: 1, HistoryFloor: writerTime.Add(-time.Hour), HistoryCeiling: writerTime.Add(time.Hour), CoverageGeneration: 1}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, string(raw)))
	f := &importFixture{st: st, sourceID: source.ID, clock: newTestClock(), conversations: map[string][]Media{}, pageSize: 1}
	// An old own root with no cached replies must still enter a full sweep.
	root := writerMedia("300", "Old root")
	root.HasReplies = new(false)
	applyWriter(t, w, root, ItemOrigin{Kind: "own_posts"})
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}
func (f *importFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.Path)
	if f.failure != nil && f.failure(w, r) {
		return
	}
	var items []Media
	switch r.URL.Path {
	case "/v1.0/me/threads":
		items = f.posts
	case "/v1.0/me/replies":
		items = f.replies
	default:
		root := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.0/"), "/conversation")
		items = f.conversations[root]
	}
	if r.URL.Query().Has("since") {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		until, _ := strconv.ParseInt(r.URL.Query().Get("until"), 10, 64)
		filtered := []Media{}
		for _, m := range items {
			if m.Timestamp.Unix() >= since && m.Timestamp.Unix() <= until {
				filtered = append(filtered, m)
			}
		}
		items = filtered
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("after"))
	end := min(len(items), offset+f.pageSize)
	data := []map[string]any{}
	if offset < len(items) {
		for _, m := range items[offset:end] {
			row := map[string]any{"id": m.ID, "timestamp": m.Timestamp.Format(time.RFC3339Nano)}
			if m.Text != nil {
				row["text"] = *m.Text
			}
			if m.Username != nil {
				row["username"] = *m.Username
			}
			if m.RootPost != nil {
				row["root_post"] = m.RootPost
			}
			if m.RepliedTo != nil {
				row["replied_to"] = m.RepliedTo
			}
			if m.IsReply != nil {
				row["is_reply"] = m.IsReply
			}
			data = append(data, row)
		}
	}
	body, err := json.Marshal(map[string]any{"data": data, "paging": map[string]any{"cursors": map[string]any{"after": strconv.Itoa(end)}}})
	if err != nil {
		http.Error(w, "fixture encoding", 500)
		return
	}
	respond(w, 200, string(body))
}
func (f *importFixture) importer(t *testing.T, cap int) (*Importer, ImportOptions) {
	t.Helper()
	budget := f.clock.budget(cap)
	c, err := NewClient(ClientOptions{HTTPClient: f.server.Client(), BaseURL: f.server.URL + "/v1.0", AccessToken: testToken, Budget: budget})
	require.NoError(t, err)
	return NewImporter(f.st, c), ImportOptions{SourceID: f.sourceID, SourceIdentifier: writerSource, Budget: budget, Now: f.clock.Now}
}
func TestThreadsImportCapAndReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImportFixture(t)
	f.conversations["300"] = []Media{writerReply("301", "300", "300"), writerReply("302", "300", "301")}
	f.posts = []Media{writerMedia("100", "new root"), writerMedia("200", "history root")}
	f.posts[0].Timestamp = f.clock.Now().Add(-time.Hour)
	var completed bool
	for run := 0; run < 30; run++ {
		imp, opts := f.importer(t, 1)
		sum, err := imp.Import(t.Context(), opts)
		require.NoError(err)
		require.NotNil(sum)
		assert.LessOrEqual(opts.Budget.Attempts(), 1)
		if sum.Status == store.SyncStatusCompleted {
			completed = true
			assert.True(sum.HistoryComplete)
			break
		}
		assert.Equal(store.SyncStatusPaused, sum.Status)
	}
	require.True(completed, "finite pages must complete across capped runs")
	rows, err := f.st.SourceMessageMetadata(f.sourceID)
	require.NoError(err)
	ids := []string{}
	for _, row := range rows {
		var native string
		require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT source_message_id FROM messages WHERE id=?`), row.ID).Scan(&native))
		ids = append(ids, native)
	}
	assert.ElementsMatch([]string{"100", "200", "300", "301", "302"}, ids)
	var linked int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND reply_to_message_id IS NOT NULL`), f.sourceID).Scan(&linked))
	assert.Equal(2, linked)
	source, err := f.st.GetSourceByIDContext(t.Context(), f.sourceID)
	require.NoError(err)
	assert.True(source.SyncCursor.Valid)
}
func TestThreadsModesRejectBeforeIO(t *testing.T) {
	f := newImportFixture(t)
	for _, r := range []SyncRequest{{PostID: "300", Full: true}, {PostID: "300", After: writerTime}, {After: time.Unix(1688540399, 0)}, {PostID: "999"}} {
		imp, opts := f.importer(t, 10)
		opts.Request = r
		_, err := imp.Import(context.Background(), opts)
		require.Error(t, err, fmt.Sprint(r))
	}
	assert.Empty(t, f.calls)
}
