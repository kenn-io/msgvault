package msmail

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// fakeGraph is a Graph mail server that applies what it is given. It holds
// folders and messages, and a change log. A deltaLink encodes a position in
// that log, so the next delta returns only the changes made after it.
type fakeGraph struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	folders  []string          // folder IDs, in list order
	folder   map[string]string // message ID -> folder ID; absent when deleted
	log      []change          // one entry per change
	expired  map[string]bool   // folder IDs whose next delta answers 410
	throttle bool              // answer the next $value with 429 once
	pageSize int
	stopAt   int // fail the delta page at this skip offset, when non-zero

	mimeCalls  atomic.Int32
	walkStarts atomic.Int32 // delta requests with no token and no nextLink
}

// change records a message and the folder it was in before the change.
type change struct{ id, from string }

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	f := &fakeGraph{t: t, folder: map[string]string{}, expired: map[string]bool{}, pageSize: 2}
	f.folders = []string{"inbox", "archive"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) put(id, folder string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, change{id, f.folder[id]})
	f.folder[id] = folder
}

func (f *fakeGraph) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, change{id, f.folder[id]})
	delete(f.folder, id)
}

func raw(id string) string {
	return "From: a@example.com\r\nTo: me@example.com\r\nSubject: " + id +
		"\r\nMessage-ID: <" + id + "@example.com>\r\nDate: Mon, 1 Jan 2024 10:00:00 +0000\r\n\r\nbody " + id + "\r\n"
}

func (f *fakeGraph) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(f.t, json.MarshalWrite(w, v))
}

func (f *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	q := r.URL.Query()
	switch {
	case p == "/me/mailFolders":
		var out []map[string]any
		for _, id := range f.folders {
			out = append(out, map[string]any{"id": id, "displayName": strings.ToUpper(id[:1]) + id[1:]})
		}
		f.writeJSON(w, map[string]any{"value": out})
	case strings.HasPrefix(p, "/me/mailFolders/") && strings.HasSuffix(p, "/messages/delta"):
		f.delta(w, strings.Split(p, "/")[3], q)
	case strings.HasPrefix(p, "/me/mailFolders/"):
		id := strings.TrimPrefix(p, "/me/mailFolders/")
		if id == "inbox" || id == "archive" {
			f.writeJSON(w, map[string]any{"id": id})
			return
		}
		http.Error(w, `{"error":{"code":"ErrorFolderNotFound"}}`, http.StatusNotFound)
	case strings.HasPrefix(p, "/me/messages/") && strings.HasSuffix(p, "/$value"):
		f.mimeCalls.Add(1)
		if f.throttle {
			f.throttle = false
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		id := strings.Split(p, "/")[3]
		if _, ok := f.folder[id]; !ok {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(raw(id))) //nolint:gosec // local test server returns fixture MIME
	case strings.HasPrefix(p, "/me/messages/"):
		id := strings.TrimPrefix(p, "/me/messages/")
		folder, ok := f.folder[id]
		if !ok {
			http.Error(w, `{"error":{"code":"ErrorItemNotFound"}}`, http.StatusNotFound)
			return
		}
		f.writeJSON(w, map[string]any{"parentFolderId": folder})
	default:
		http.Error(w, "unexpected "+p, http.StatusBadRequest)
	}
}

// delta answers a walk (no token) or a round (token = log position).
func (f *fakeGraph) delta(w http.ResponseWriter, folder string, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	link := func(kind string, vals ...string) string {
		return f.srv.URL + "/me/mailFolders/" + folder + "/messages/delta?" + kind + "&" + strings.Join(vals, "&")
	}
	if tok := get("token"); tok != "" {
		if f.expired[folder] {
			delete(f.expired, folder)
			http.Error(w, `{"error":{"code":"syncStateNotFound"}}`, http.StatusGone)
			return
		}
		pos, _ := strconv.Atoi(tok)
		seen := map[string]bool{}
		var out []map[string]any
		for _, c := range f.log[pos:] {
			id := c.id
			if seen[id] || (c.from != folder && f.folder[id] != folder) {
				continue
			}
			seen[id] = true
			if f.folder[id] == folder {
				out = append(out, map[string]any{"id": id})
			} else {
				out = append(out, map[string]any{"id": id, "@removed": map[string]any{"reason": "deleted"}})
			}
		}
		f.writeJSON(w, map[string]any{"value": out, "@odata.deltaLink": link("t", "token="+strconv.Itoa(len(f.log)))})
		return
	}
	// A walk lists the folder in log order. pos pins the log position that
	// the final deltaLink carries, taken when the walk started.
	pos := get("pos")
	if pos == "" {
		pos = strconv.Itoa(len(f.log))
	}
	skip, _ := strconv.Atoi(get("skip"))
	if get("skip") == "" {
		f.walkStarts.Add(1)
	}
	if f.stopAt != 0 && skip == f.stopAt {
		f.stopAt = 0
		http.Error(w, "boom", http.StatusBadRequest)
		return
	}
	var ids []string
	for _, c := range f.log {
		if id := c.id; f.folder[id] == folder && !slices.Contains(ids, id) {
			ids = append(ids, c.id)
		}
	}
	end := min(skip+f.pageSize, len(ids))
	var out []map[string]any
	for _, id := range ids[skip:end] {
		out = append(out, map[string]any{"id": id, "receivedDateTime": "2024-01-01T10:00:00Z"})
	}
	resp := map[string]any{"value": out}
	if end < len(ids) {
		resp["@odata.nextLink"] = link("w", "skip="+strconv.Itoa(end), "pos="+pos)
	} else {
		resp["@odata.deltaLink"] = link("t", "token="+pos)
	}
	f.writeJSON(w, resp)
}

func (f *fakeGraph) sync(t *testing.T, st *store.Store) (*Summary, error) {
	t.Helper()
	c := NewClient(f.srv.URL, func(context.Context) (string, error) { return "tok", nil }, 1000)
	return Import(context.Background(), st, c, Options{Email: "me@example.com"}, slog.Default())
}

// state returns message ID -> "folder label name" or "deleted".
func state(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.DB().Query(`
		SELECT m.source_message_id, COALESCE(l.name, ''), m.deleted_from_source_at IS NOT NULL
		FROM messages m
		LEFT JOIN message_labels ml ON ml.message_id = m.id
		LEFT JOIN labels l ON l.id = ml.label_id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, label string
		var deleted bool
		require.NoError(t, rows.Scan(&id, &label, &deleted))
		if deleted {
			label = "deleted"
		}
		_, dup := out[id]
		assert.False(t, dup, "message %s has more than one row or label", id)
		out[id] = label
	}
	require.NoError(t, rows.Err())
	return out
}

func TestImportFirstSyncThenNoChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	f.put("m2", "inbox")
	f.put("m3", "inbox")
	f.put("m4", "archive")
	f.throttle = true

	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(4, sum.Added)
	assert.Equal(map[string]string{"m1": "Inbox", "m2": "Inbox", "m3": "Inbox", "m4": "Archive"}, state(t, st))
	assert.EqualValues(5, f.mimeCalls.Load(), "four downloads plus one 429")

	var labelType string
	require.NoError(st.DB().QueryRow(`SELECT label_type FROM labels WHERE source_label_id = 'inbox'`).Scan(&labelType))
	assert.Equal("system", labelType)

	f.mimeCalls.Store(0)
	sum, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(0, sum.Added)
	assert.EqualValues(0, f.mimeCalls.Load())
}

func TestImportMoveAndDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	f.put("m2", "inbox")
	f.put("m3", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.put("m1", "archive") // move
	f.remove("m2")         // hard delete
	f.put("m5", "inbox")   // new
	f.mimeCalls.Store(0)

	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "Archive", "m2": "deleted", "m3": "Inbox", "m5": "Inbox"}, state(t, st))
	assert.Equal(1, sum.Added)
	assert.Equal(1, sum.Moved)
	assert.Equal(1, sum.Deleted)
	assert.EqualValues(1, f.mimeCalls.Load())
}

func TestImportResumesFromCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	for i := range 5 {
		f.put(fmt.Sprintf("m%d", i), "inbox")
	}
	f.stopAt = 4 // the third page fails

	_, err := f.sync(t, st)
	require.Error(err)
	assert.Len(state(t, st), 4)

	f.walkStarts.Store(0)
	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, sum.Added)
	assert.EqualValues(1, f.walkStarts.Load(), "only archive starts a walk; inbox resumes at the saved nextLink")
	assert.Len(state(t, st), 5)
}

func TestImportExpiredTokenWalksAgain(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	f.put("m2", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.put("m3", "inbox")
	f.expired["inbox"] = true
	f.mimeCalls.Store(0)
	f.walkStarts.Store(0)

	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, sum.Added)
	assert.EqualValues(1, f.mimeCalls.Load())
	assert.EqualValues(1, f.walkStarts.Load(), "inbox walks again")
	assert.Equal(map[string]string{"m1": "Inbox", "m2": "Inbox", "m3": "Inbox"}, state(t, st))
}
