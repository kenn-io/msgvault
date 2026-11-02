package threads

import (
	"context"
	"database/sql"
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
		http.Error(w, "fixture encoding", http.StatusInternalServerError)
		return
	}
	respond(w, 200, string(body))
}
func (f *importFixture) importer(t *testing.T, attemptLimit int) (*Importer, ImportOptions) {
	t.Helper()
	budget := f.clock.budget(attemptLimit)
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
	for range 30 {
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

func TestThreadsMaintainerEmptyContinuationArchivesLaterReply(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newImportFixture(t)
	f.failure = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1.0/300/conversation" {
			return false
		}
		switch r.URL.Query().Get("after") {
		case "":
			respond(w, 200, `{"data":[],"paging":{"cursors":{"after":"A"},"next":"https://untrusted.example/next?access_token=synthetic"}}`)
		case "A":
			respond(w, 200, fmt.Sprintf(`{"data":[{"id":"301","timestamp":%q,"root_post":{"id":"300"},"replied_to":{"id":"300"},"text":"Synthetic later reply"}],"paging":{"cursors":{"after":"B"}}}`, writerTime.Format(time.RFC3339)))
		case "B":
			respond(w, 200, `{"data":[]}`)
		default:
			assert.Fail("unexpected continuation")
			respond(w, 400, `{}`)
		}
		return true
	}
	imp, opts := f.importer(t, 100)
	opts.Request.PostID = "300"
	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, sum.Status)
	_, found, err := f.st.ThreadsArchiveMessageMetadataContext(t.Context(), f.sourceID, "301")
	require.NoError(err)
	assert.True(found, "an empty page with an explicit continuation must not hide later replies")
}

func TestThreadsMaintainerThirdExpiredCursorCanResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newImportFixture(t)
	starts := 0
	f.failure = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1.0/300/conversation" {
			return false
		}
		after := r.URL.Query().Get("after")
		if strings.HasPrefix(after, "expired-") {
			respond(w, 400, `{"error":{"code":100,"message":"The paging cursor has expired"}}`)
		} else if after == "fresh" {
			respond(w, 200, `{"data":[]}`)
		} else {
			starts++
			next := "fresh"
			if starts <= 3 {
				next = fmt.Sprintf("expired-%d", starts)
			}
			respond(w, 200, fmt.Sprintf(`{"data":[{"id":"301","timestamp":%q,"root_post":{"id":"300"},"replied_to":{"id":"300"}}],"paging":{"cursors":{"after":%q}}}`, writerTime.Format(time.RFC3339), next))
		}
		return true
	}
	imp, opts := f.importer(t, 100)
	opts.Request.PostID = "300"
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.Equal(store.SyncStatusPaused, first.Status)
	assert.Positive(first.Incomplete)
	imp, opts = f.importer(t, 100)
	opts.Request.PostID = "300"
	resumed, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, resumed.Status, "a later run must restart pinned bounds rather than retry a permanently expired cursor")
	assert.GreaterOrEqual(starts, 4)
}

func TestThreadsMaintainerDefiniteCreateRejectionReleasesDraft(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		for _, action := range []string{"edit", "delete"} {
			t.Run(strconv.Itoa(status)+"/"+action, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				f := newSendFixture(t)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1.0/me/threads_publishing_limit":
						respond(w, 200, `{"data":[{"reply_quota_usage":0,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`)
					case "/v1.0/me/threads":
						respond(w, status, `{"error":{"code":100,"message":"Invalid reply text"}}`)
					default:
						assert.Fail("unexpected provider call", r.URL.Path)
						respond(w, 500, `{}`)
					}
				}))
				t.Cleanup(server.Close)
				var err error
				f.api, err = NewClient(ClientOptions{BaseURL: server.URL + "/v1.0", HTTPClient: server.Client(), AccessToken: testToken, Budget: f.clock.budget(100)})
				require.NoError(err)
				_, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
				require.Error(err)
				var rejected *APIError
				require.ErrorAs(err, &rejected)
				assert.Equal(status, rejected.StatusCode, "exercise the actual provider response")
				if action == "edit" {
					edited, editErr := f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "Synthetic corrected reply")
					require.NoError(editErr, "a definite provider rejection must release the draft")
					assert.Equal(int64(2), edited.Revision)
				} else {
					require.NoError(f.st.DeleteChatDraftContext(t.Context(), f.draft.DraftID, 1), "a definite provider rejection must permit deletion")
				}
			})
		}
	}
}

func TestThreadsParentRepairYieldsBeforeDrainingBacklog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	w, st := writerFixture(t)
	// All children arrive before their parent. A repair boundary must yield
	// while eligible work remains, without relying on runner throughput.
	for n := range 201 {
		applyWriter(t, w, writerReply(strconv.Itoa(1000+n), "100", "101"), ItemOrigin{Kind: "conversation", RootID: "100"})
	}
	parent := applyWriter(t, w, writerReply("101", "100", "100"), ItemOrigin{Kind: "conversation", RootID: "100"})
	require.NoError(w.ResolveParents(t.Context()))
	var linked int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND reply_to_message_id=?`), w.sourceID, parent).Scan(&linked))
	assert.Positive(linked)
	assert.Less(linked, 201, "a single repair boundary must leave finite work for later boundaries")
	// Ingestion can continue between repair batches.
	applyWriter(t, w, writerMedia("9000", "Newly ingested post"), ItemOrigin{Kind: "own_posts"})
	require.NoError(w.ResolveParents(t.Context()))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND reply_to_message_id=?`), w.sourceID, parent).Scan(&linked))
	assert.Equal(201, linked, "later boundaries must finish pending repair")
}

func TestThreadsMergedArchiveOnlyRepliesDoNotBlockRepair(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(strconv.FormatBool(present), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			live, st := writerFixture(t)
			history, err := st.GetOrCreateSource("threads", "threads:123:789")
			require.NoError(err)
			archived := NewWriter(st, history.ID, history.Identifier)
			child := applyWriter(t, archived, writerReply("101", "100", "200"), ItemOrigin{Kind: "conversation", RootID: "100"})
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.sourceID})
			require.NoError(err)
			if present {
				applyWriter(t, live, writerMedia("200", "Native parent in a different conversation"), ItemOrigin{Kind: "own_posts"})
			}
			require.NoError(live.ResolveParents(t.Context()))
			link, err := st.GetMessageReplyToMessageIDContext(t.Context(), child)
			require.NoError(err)
			assert.False(link.Valid, "merged history must stay archive-only even when a native ID matches its parent")
			applyWriter(t, live, writerMedia("300", "Ingestion continues"), ItemOrigin{Kind: "own_posts"})
		})
	}
}

func TestThreadsParentRepairScanWrapsAcrossWriters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newImportFixture(t)
	source, err := f.st.GetSourceByID(f.sourceID)
	require.NoError(err)
	coverage := source.SyncConfig
	live := NewWriter(f.st, f.sourceID, source.Identifier)
	otherSource, err := f.st.GetOrCreateSource("threads", "threads:123:789")
	require.NoError(err)
	other := NewWriter(f.st, otherSource.ID, otherSource.Identifier)
	otherChild := applyWriter(t, other, writerReply("501", "500", "502"), ItemOrigin{Kind: "conversation", RootID: "500"})
	otherParent := applyWriter(t, other, writerReply("502", "500", "500"), ItemOrigin{Kind: "conversation", RootID: "500"})
	var first int64
	for n := range 201 {
		id := applyWriter(t, live, writerReply(strconv.Itoa(1000+n), "300", "9999"), ItemOrigin{Kind: "conversation", RootID: "300"})
		if n == 0 {
			first = id
		}
	}
	require.NoError(live.ResolveParents(t.Context()))
	// A canceled boundary cannot lose the completed durable scan position.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(NewWriter(f.st, f.sourceID, source.Identifier).ResolveParents(canceled), context.Canceled)
	// This source's earlier row IDs must not inherit the first source's cursor.
	require.NoError(NewWriter(f.st, otherSource.ID, otherSource.Identifier).ResolveParents(t.Context()))
	link, err := f.st.GetMessageReplyToMessageIDContext(t.Context(), otherChild)
	require.NoError(err)
	assert.Equal(sql.NullInt64{Int64: otherParent, Valid: true}, link)
	parent := applyWriter(t, live, writerReply("9999", "300", "300"), ItemOrigin{Kind: "conversation", RootID: "300"})
	require.NoError(NewWriter(f.st, f.sourceID, source.Identifier).ResolveParents(t.Context()))
	link, err = f.st.GetMessageReplyToMessageIDContext(t.Context(), first)
	require.NoError(err)
	assert.False(link.Valid, "a new writer resumes after the missing-parent prefix before wrapping")
	require.NoError(NewWriter(f.st, f.sourceID, source.Identifier).ResolveParents(t.Context()))
	link, err = f.st.GetMessageReplyToMessageIDContext(t.Context(), first)
	require.NoError(err)
	assert.Equal(sql.NullInt64{Int64: parent, Valid: true}, link, "wraparound repairs children whose parent arrived later")
	source, err = f.st.GetSourceByID(f.sourceID)
	require.NoError(err)
	assert.Equal(coverage, source.SyncConfig, "repair scanning cannot move pinned provider coverage")
}

func TestThreadsEmptyContinuationRetainsRepeatedPageProtection(t *testing.T) {
	require := require.New(t)
	f := newImportFixture(t)
	f.failure = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1.0/300/conversation" {
			return false
		}
		switch r.URL.Query().Get("after") {
		case "", "B":
			after := "A"
			if r.URL.Query().Get("after") == "B" {
				after = "C"
			}
			respond(w, http.StatusOK, fmt.Sprintf(`{"data":[{"id":"301","timestamp":%q,"root_post":{"id":"300"},"replied_to":{"id":"300"}}],"paging":{"cursors":{"after":%q}}}`, writerTime.Format(time.RFC3339), after))
		case "A":
			respond(w, http.StatusOK, `{"data":[],"paging":{"cursors":{"after":"B"},"next":"https://untrusted.example/next"}}`)
		default:
			respond(w, http.StatusOK, `{"data":[]}`)
		}
		return true
	}
	imp, opts := f.importer(t, 100)
	opts.Request.PostID = "300"
	_, err := imp.Import(t.Context(), opts)
	require.ErrorIs(err, ErrPagination, "an empty continuation cannot erase repeated-page evidence")
}
