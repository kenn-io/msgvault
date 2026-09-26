package msmail

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// fakeGraph is a Graph mail server that applies what it is given. It holds
// folders and messages, and a change log. A deltaLink encodes a position in
// that log, so the next delta returns only the changes made after it.
type fakeGraph struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	folders []string          // folder IDs, in list order
	folder  map[string]string // message ID -> folder ID; absent when deleted
	log     []change          // one entry per change
	expired map[string]bool   // folder IDs whose next delta answers 410
	gone    map[string]bool   // folder IDs whose every delta answers 410
	version map[string]int    // message ID -> content version

	withAttachment map[string]bool   // message IDs whose MIME carries a file
	shifted        map[string]bool   // message IDs whose file moves to another part
	attachmentBody map[string]string // message ID -> base64 file content
	broken         map[string]bool   // message IDs whose MIME does not parse
	goneOnValue    map[string]bool   // message IDs deleted just before their $value
	attachDir      string            // attachments directory; a fresh one when empty
	throttle       bool              // answer the next $value with 429 once
	pageSize       int
	stopAt         int // fail the delta page at this skip offset, when non-zero

	mimeCalls  atomic.Int32
	walkStarts atomic.Int32 // delta requests with no token and no nextLink
}

// change records a message and the folder it was in before the change.
type change struct{ id, from string }

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	f := &fakeGraph{t: t, folder: map[string]string{}, expired: map[string]bool{}, gone: map[string]bool{}, version: map[string]int{}, withAttachment: map[string]bool{}, shifted: map[string]bool{}, attachmentBody: map[string]string{}, broken: map[string]bool{}, goneOnValue: map[string]bool{}, pageSize: 2}
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

func raw(id string, version int) string {
	return "From: a@example.com\r\nTo: me@example.com\r\nSubject: " + id +
		"\r\nMessage-ID: <" + id + "@example.com>\r\nDate: Mon, 1 Jan 2024 10:00:00 +0000\r\n\r\nbody " + id +
		" v" + strconv.Itoa(version) + "\r\n"
}

// rawWithAttachment carries one file. When shifted, a second text part comes
// first, so the file gets another MIME part key.
func rawWithAttachment(id string, shifted bool, content string) string {
	switch content {
	case "":
		content = "aGVsbG8="
	case "-":
		content = "" // an empty file
	}
	extra := ""
	if shifted {
		extra = "--b\r\nContent-Type: text/plain\r\n\r\nnote\r\n"
	}
	return "From: a@example.com\r\nTo: me@example.com\r\nSubject: " + id +
		"\r\nMessage-ID: <" + id + "@example.com>\r\nDate: Mon, 1 Jan 2024 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody " + id + "\r\n" + extra +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=a.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + content + "\r\n--b--\r\n"
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
		if slices.Contains(f.folders, id) {
			f.writeJSON(w, map[string]any{"id": id})
			return
		}
		if id == "recoverableitemsdeletions" {
			f.writeJSON(w, map[string]any{"id": "deletions"})
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
		if f.goneOnValue[id] {
			delete(f.folder, id)
		}
		if _, ok := f.folder[id]; !ok {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		body := raw(id, f.version[id])
		if f.withAttachment[id] {
			body = rawWithAttachment(id, f.shifted[id], f.attachmentBody[id])
		}
		if f.broken[id] {
			body = "From: a@example.com\r\nSubject: " + id + "\r\nMIME-Version: 1.0\r\n" +
				"Content-Type: multipart mixed; boundary=b\r\n\r\n--b\r\n\r\nbody\r\n--b--\r\n"
		}
		_, _ = w.Write([]byte(body)) //nolint:gosec // local test server returns fixture MIME
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
	if f.gone[folder] {
		http.Error(w, `{"error":{"code":"syncStateNotFound"}}`, http.StatusGone)
		return
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
	dir := f.attachDir
	if dir == "" {
		dir = f.t.TempDir()
	}
	return Import(context.Background(), st, c, Options{Email: "me@example.com", AttachmentsDir: dir}, slog.Default())
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

	f.put("m1", "archive")   // move
	f.remove("m2")           // purged
	f.put("m3", "deletions") // Shift+Delete: hidden Recoverable Items
	f.put("m5", "inbox")     // new
	f.mimeCalls.Store(0)

	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "Archive", "m2": "deleted", "m3": "deleted", "m5": "Inbox"}, state(t, st))
	assert.Equal(1, sum.Added)
	assert.Equal(1, sum.Moved)
	assert.Equal(2, sum.Deleted)
	assert.EqualValues(2, f.mimeCalls.Load(), "the new message, and the moved one again")
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
	assert.EqualValues(2, f.walkStarts.Load(), "inbox starts its interrupted walk over; archive starts its first")
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

// snippets returns message ID -> snippet, the stored body start.
func snippets(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT source_message_id, COALESCE(snippet, '') FROM messages`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, snippet string
		require.NoError(t, rows.Scan(&id, &snippet))
		out[id] = snippet
	}
	require.NoError(t, rows.Err())
	return out
}

// An incremental round stores every changed message again. A walk after an
// expired cursor returns every message, so it downloads again only drafts,
// which keep their ID while they are edited.
func TestImportRefreshesChangedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.folders = append(f.folders, "drafts")
	f.put("d1", "drafts")
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.version["d1"], f.version["m1"] = 1, 1
	f.put("d1", "drafts")
	f.put("m1", "inbox")
	f.mimeCalls.Store(0)
	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(2, sum.Updated)
	assert.EqualValues(2, f.mimeCalls.Load())
	assert.Equal(map[string]string{"d1": "body d1 v1", "m1": "body m1 v1"}, snippets(t, st))

	f.version["d1"], f.version["m1"] = 2, 2
	f.expired["inbox"], f.expired["drafts"] = true, true
	f.mimeCalls.Store(0)
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.EqualValues(1, f.mimeCalls.Load(), "the walk downloads only the draft again")
	assert.Equal(map[string]string{"d1": "body d1 v2", "m1": "body m1 v1"}, snippets(t, st))
}

// A message stored again replaces the attachments of its old MIME.
func TestImportRefreshReplacesAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.withAttachment["m1"] = true
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal([2]int{1, 1}, attachments(t, st))

	f.withAttachment["m1"] = false
	f.put("m1", "inbox")
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal([2]int{0, 0}, attachments(t, st))
}

// attachments returns the attachment row count and attachment_count of m1.
func attachments(t *testing.T, st *store.Store) [2]int {
	t.Helper()
	var rows, count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT (SELECT COUNT(*) FROM attachments a WHERE a.message_id = m.id), m.attachment_count
		FROM messages m WHERE m.source_message_id = 'm1'`)).Scan(&rows, &count))
	return [2]int{rows, count}
}

// A message deleted while no cursor covered its folder is marked deleted when
// the walk after an expired cursor does not return it.
func TestImportExpiredTokenReconcilesMissedDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	f.put("m2", "inbox")
	f.put("m3", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.remove("m2")         // purged during the gap
	f.put("m3", "archive") // moved during the gap
	f.expired["inbox"] = true
	f.expired["archive"] = true
	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "Inbox", "m2": "deleted", "m3": "Archive"}, state(t, st))
	assert.Equal(1, sum.Deleted)
}

// A message restored from Recoverable Items loses its deletion mark.
func TestImportRestoredMessageClearsDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.put("m1", "deletions")
	_, err = f.sync(t, st)
	require.NoError(err)
	require.Equal(map[string]string{"m1": "deleted"}, state(t, st))

	f.put("m1", "inbox")
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "Inbox"}, state(t, st))
}

// A folder that answers 410 even to a fresh walk fails the sync instead of
// walking again forever.
func TestImportRepeatedGoneFails(t *testing.T) {
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.gone["inbox"] = true
	_, err := f.sync(t, st)
	require.ErrorIs(t, err, msgraph.ErrGone)
}

// A message that fails to store stops the sync before its page cursor is
// saved, so the next sync stores it.
func TestImportStoreFailureDoesNotAdvanceCursor(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to fail one insert")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	f.put("m2", "inbox")
	_, err := st.DB().Exec(`CREATE TRIGGER fail_m2 BEFORE INSERT ON messages
		WHEN NEW.source_message_id = 'm2' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	require.NoError(err)

	_, err = f.sync(t, st)
	require.Error(err)

	_, err = st.DB().Exec(`DROP TRIGGER fail_m2`)
	require.NoError(err)
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "Inbox", "m2": "Inbox"}, state(t, st))
}

// A walk after an expired cursor that is interrupted starts over on the next
// sync, so its end still finds the messages that left during the gap.
func TestImportInterruptedRewalkStartsOver(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	for i := range 5 {
		f.put(fmt.Sprintf("m%d", i), "inbox")
	}
	_, err := f.sync(t, st)
	require.NoError(err)

	f.remove("m0") // purged during the gap
	f.expired["inbox"] = true
	f.stopAt = 2 // the second page of the walk fails
	_, err = f.sync(t, st)
	require.Error(err)

	f.walkStarts.Store(0)
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.EqualValues(1, f.walkStarts.Load(), "inbox walks again from the start")
	assert.Equal("deleted", state(t, st)["m0"])
}

// When the attachment of a refreshed message cannot be written, the row of
// the old MIME stays, and the next sync downloads the message again.
func TestImportRefreshKeepsAttachmentWhenWriteFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.withAttachment["m1"] = true
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(blocker, nil, 0o600))
	f.attachDir = filepath.Join(blocker, "attachments") // cannot be created
	f.shifted["m1"] = true                              // the file gets a new part key
	f.put("m1", "inbox")
	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, sum.Errors)
	assert.Equal([2]int{1, 1}, attachments(t, st))

	f.attachDir = ""
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal([2]int{1, 1}, attachments(t, st))
	assert.Equal("mime:3", attachmentKey(t, st), "the old row is replaced by the new part")
}

// A refreshed part with the same key but new bytes that cannot be written is
// retried on the next sync, so the old content is not kept as if current.
func TestImportRefreshRetriesChangedPartNotWritten(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.withAttachment["m1"] = true
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)
	before := attachmentHash(t, st)

	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(blocker, nil, 0o600))
	f.attachDir = filepath.Join(blocker, "attachments") // cannot be created
	f.attachmentBody["m1"] = "d29ybGQ="                 // same part, new bytes
	f.put("m1", "inbox")
	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, sum.Errors)

	f.attachDir = ""
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.NotEqual(before, attachmentHash(t, st))
	assert.Equal([2]int{1, 1}, attachments(t, st))
}

func attachmentKey(t *testing.T, st *store.Store) string {
	t.Helper()
	var key string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT a.source_part_key FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = 'm1'`)).Scan(&key))
	return key
}

func attachmentHash(t *testing.T, st *store.Store) string {
	t.Helper()
	var hash string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT a.content_hash FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = 'm1'`)).Scan(&hash))
	return hash
}

// A new message whose attachment cannot be written is downloaded again on the
// next sync, even though a walk skips known messages.
func TestImportNewMessageAttachmentWriteFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(blocker, nil, 0o600))
	f.attachDir = filepath.Join(blocker, "attachments") // cannot be created
	f.withAttachment["m1"] = true
	f.put("m1", "inbox")
	sum, err := f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, sum.Errors)
	assert.Equal([2]int{0, 0}, attachments(t, st))

	f.attachDir = ""
	f.expired["inbox"] = true // the next sync walks, and a walk skips known messages
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal([2]int{1, 1}, attachments(t, st))
}

// Storage skips an empty attachment on purpose, so it does not fail the sync.
func TestImportEmptyAttachmentDoesNotFail(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.withAttachment["m1"] = true
	f.attachmentBody["m1"] = "-"
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.put("m1", "inbox") // refresh
	_, err = f.sync(t, st)
	require.NoError(err)
}

// A folder removed from the mailbox is retired: its messages that moved get
// their new folder, and the ones that are gone are marked deleted.
func TestImportRemovedFolderIsRetired(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.folders = append(f.folders, "old")
	f.put("m1", "old")
	f.put("m2", "old")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.folders = []string{"inbox", "archive"} // "old" is removed
	f.remove("m1")
	f.folder["m2"] = "inbox" // moved before the removal, with no change log entry
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "deleted", "m2": "Inbox"}, state(t, st))
}

// A known message that delta reports but that is deleted before its $value is
// looked up and marked deleted.
func TestImportKnownMessageGoneBeforeDownload(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.put("m1", "inbox") // changed, so the round downloads it again
	f.goneOnValue["m1"] = true
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(map[string]string{"m1": "deleted"}, state(t, st))
}

// A refreshed MIME that does not parse keeps the attachment rows there.
func TestImportRefreshWithBrokenMIMEKeepsAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.withAttachment["m1"] = true
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)

	f.broken["m1"] = true
	f.put("m1", "inbox")
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(1, attachments(t, st)[0])
}
