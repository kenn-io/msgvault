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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A real Graph response adds provider state to the existing transport fixture;
// omission on later responses must preserve the last authoritative snapshot.
func TestProviderStateReadFollowupAndMove(t *testing.T) {
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	st := testutil.NewTestStore(t)
	var state atomic.Value
	state.Store(map[string]any{"isRead": false, "flag": map[string]any{"flagStatus": "flagged"}})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.serve(rec, r)
		body := rec.Body.Bytes()
		if strings.Contains(r.URL.Path, "/messages") && !strings.HasSuffix(r.URL.Path, "/$value") && rec.Code == http.StatusOK {
			if r.URL.Query().Get("token") == "" && r.URL.Query().Get("skip") == "" {
				fields := strings.Split(r.URL.Query().Get("$select"), ",")
				assert.Contains(t, fields, "isRead", "native Graph acquisition must request read state")
				assert.Contains(t, fields, "flag", "native Graph acquisition must request follow-up state")
			}
			var payload map[string]any
			require.NoError(t, json.Unmarshal(body, &payload))
			apply := func(item map[string]any) {
				for k, v := range state.Load().(map[string]any) {
					item[k] = v
				}
			}
			if values, ok := payload["value"].([]any); ok {
				for _, value := range values {
					apply(value.(map[string]any))
				}
			} else {
				apply(payload)
			}
			var err error
			body, err = json.Marshal(payload)
			require.NoError(t, err)
		}
		for k, values := range rec.Header() {
			w.Header()[k] = values
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	f.srv = proxy // keep opaque delta links on the same fixture transport
	c := NewClient(proxy.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
	run := func() {
		t.Helper()
		_, err := Import(context.Background(), st, c, Options{Email: "sample@example.test", AttachmentsDir: t.TempDir()}, slog.Default())
		require.NoError(t, err)
	}
	run()
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "sample@example.test")
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(t, err)
	labels := func() []string {
		t.Helper()
		rows, err := st.DB().Query(st.Rebind(`SELECT l.source_label_id FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_id=?`), ids["m1"])
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var result []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			result = append(result, id)
		}
		require.NoError(t, rows.Err())
		return result
	}
	assert.ElementsMatch(t, []string{"inbox", "msmail-state:unread", "msmail-state:flagged"}, labels())
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=TRUE WHERE id=?`), ids["m1"])
	require.NoError(t, err)
	state.Store(map[string]any{})
	f.put("m1", "archive")
	run()
	assert.ElementsMatch(t, []string{"archive", "msmail-state:unread", "msmail-state:flagged"}, labels())
	var localRead bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), ids["m1"]).Scan(&localRead))
	assert.True(t, localRead, "provider unread state must not overwrite local reader state")
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), ids["m1"])
	require.NoError(t, err)
	state.Store(map[string]any{"isRead": true, "flag": map[string]any{"flagStatus": "complete"}})
	f.put("m1", "archive")
	run()
	assert.Equal(t, []string{"archive"}, labels())
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), ids["m1"]).Scan(&localRead))
	assert.False(t, localRead, "provider read state must not overwrite local reader state")
}

func TestProviderStateLegacyCursorRestartPreservesStateAndRetryDebt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(t, err)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(t, err)
	read, flag := false, "flagged"
	_, err = st.ReconcileMicrosoftMailStateContext(t.Context(), ids["m1"], nil, nil, store.MicrosoftMailState{IsRead: &read, FlagStatus: &flag})
	require.NoError(t, err)
	legacy := map[string]string{
		"inbox":    "categories-v1:" + f.srv.URL + "/me/mailFolders/inbox/messages/delta?t&token=1",
		"archive":  "categories-v1:" + f.srv.URL + "/me/mailFolders/archive/messages/delta?t&token=1",
		"retry:m1": "inbox",
	}
	blob, err := json.Marshal(legacy)
	require.NoError(t, err)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(t, err)
	require.NoError(t, st.ScopedToSync(src.ID, run).CompleteSyncAndPreserveSourceCursorContext(t.Context(), run, src.ID, string(blob)))
	require.NoError(t, st.Close())
	reopened, err := store.Open(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	f.mu.Lock()
	f.categories = map[string][]string{"m1": {"Later"}}
	f.version["m1"] = 1
	f.mu.Unlock()
	f.put("m1", "archive")
	summary, err := f.sync(t, reopened)
	require.NoError(t, err, "old categories-v1 prefixes must be decoded before draining their URLs")
	assert.False(t, summary.Incomplete)
	message, err := reopened.GetMessage(ids["m1"])
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Archive", "Category: Later", "Provider: Unread", "Provider: Flagged"}, message.Labels)
	assert.Equal(t, "body m1 v1", message.Snippet)
	last, err := reopened.GetLastSuccessfulSync(src.ID)
	require.NoError(t, err)
	var cursors map[string]string
	require.NoError(t, json.Unmarshal([]byte(last.CursorAfter.String), &cursors))
	assert.NotContains(t, cursors, "retry:m1")
	assert.True(t, strings.HasPrefix(cursors["inbox"], "provider-state-v2:"))
	assert.True(t, strings.HasPrefix(cursors["archive"], "provider-state-v2:"))
}

func TestProviderStateCompatibleDuplicateNewObservations(t *testing.T) {
	for _, explicitFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicitFirst=%t", explicitFirst), func(t *testing.T) {
			f := newFakeGraph(t)
			f.put("m1", "inbox")
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				recorder := httptest.NewRecorder()
				f.serve(recorder, r)
				body := recorder.Body.Bytes()
				if strings.Contains(r.URL.Path, "/inbox/messages/delta") && r.URL.Query().Get("token") == "" {
					var payload map[string]any
					if !assert.NoError(t, json.Unmarshal(body, &payload)) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					explicit := map[string]any{"id": "m1", "isRead": false, "flag": map[string]any{"flagStatus": "flagged"}, "categories": []string{"Blue"}}
					absent := map[string]any{"id": "m1"}
					payload["value"] = []any{absent, explicit}
					if explicitFirst {
						payload["value"] = []any{explicit, absent}
					}
					var err error
					body, err = json.Marshal(payload)
					if !assert.NoError(t, err) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(body)
			}))
			defer proxy.Close()
			f.srv = proxy
			st := testutil.NewTestStore(t)
			summary, err := f.sync(t, st)
			require.NoError(t, err)
			assert.Equal(t, 1, summary.Added, "duplicate observations identify one new message")
			assert.EqualValues(t, 1, f.mimeCalls.Load(), "compatible partial observations need one MIME acquisition")
			src, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
			require.NoError(t, err)
			ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
			require.NoError(t, err)
			message, err := st.GetMessage(ids["m1"])
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"Inbox", "Category: Blue", "Provider: Unread", "Provider: Flagged"}, message.Labels)
		})
	}
}

func TestProviderStateUnknownDestinationRetainsDurableAcquisitionDebt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(t, err)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(t, err)
	read, flag := false, "flagged"
	_, err = st.ReconcileMicrosoftMailStateContext(t.Context(), ids["m1"], nil, nil, store.MicrosoftMailState{IsRead: &read, FlagStatus: &flag})
	require.NoError(t, err)
	f.mu.Lock()
	f.categories = map[string][]string{"m1": {"Moved"}}
	f.version["m1"] = 1
	f.mu.Unlock()
	f.put("m1", "later") // Lookup knows this destination before this run's folder list.
	summary, err := f.sync(t, st)
	require.NoError(t, err)
	assert.True(t, summary.Incomplete, "unresolved folder acquisition cannot report catchup")
	last, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(t, err)
	var cursors map[string]string
	require.NoError(t, json.Unmarshal([]byte(last.CursorAfter.String), &cursors))
	assert.Equal(t, "later", cursors["retry:m1"])
	require.NoError(t, st.Close())
	reopened, err := store.Open(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	f.mu.Lock()
	f.folders = append(f.folders, "later")
	f.names = map[string]string{"later": "Later"}
	f.mu.Unlock()
	summary, err = f.sync(t, reopened)
	require.NoError(t, err)
	assert.False(t, summary.Incomplete)
	message, err := reopened.GetMessage(ids["m1"])
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Later", "Category: Moved", "Provider: Unread", "Provider: Flagged"}, message.Labels)
	assert.Equal(t, "body m1 v1", message.Snippet)
}

func TestProviderStateDownloadIntentPrecedesMIMEAndSurvivesPostStoreFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	f.withAttachment["m1"] = true
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("fixture"), 0o600))
	f.attachDir = blocked
	src, err := st.GetOrCreateSource(SourceType, "me@example.com")
	require.NoError(t, err)
	var active atomic.Pointer[store.Store]
	active.Store(st)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/$value") {
			checkpoint, err := active.Load().GetLatestCheckpointedSyncContext(r.Context(), src.ID)
			if assert.NoError(t, err, "download intent is durable before raw bytes can commit") && assert.NotNil(t, checkpoint) {
				var pending map[string]string
				if assert.NoError(t, json.Unmarshal([]byte(checkpoint.CursorBefore.String), &pending)) {
					assert.Equal(t, "inbox", pending["retry:m1"])
				}
			}
		}
		f.serve(w, r)
	}))
	defer proxy.Close()
	f.srv = proxy
	summary, err := f.sync(t, st)
	require.NoError(t, err)
	assert.True(t, summary.Incomplete)
	last, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(t, err)
	var pending map[string]string
	require.NoError(t, json.Unmarshal([]byte(last.CursorAfter.String), &pending))
	assert.Contains(t, pending, "retry:m1")
	require.NoError(t, st.Close())
	reopened, err := store.Open(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	// The fresh importer must replay the existing durable intent, not depend on
	// an in-memory afterStore result or scan all historical MIME.
	active.Store(reopened)
	f.attachDir = t.TempDir()
	summary, err = f.sync(t, reopened)
	require.NoError(t, err)
	assert.False(t, summary.Incomplete)
	var attachments int
	require.NoError(t, reopened.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachments))
	assert.Equal(t, 1, attachments)
}

func TestProviderStateMigrationLostOldStreamRetainsMIMEDebt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(t, err)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(t, err)
	var expire atomic.Bool
	expire.Store(true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		f.serve(recorder, r)
		if strings.Contains(r.URL.Path, "/inbox/messages/delta") && r.URL.Query().Get("token") == "" && expire.Swap(false) {
			f.mu.Lock()
			f.expired["inbox"] = true
			f.mu.Unlock()
		}
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer proxy.Close()
	f.srv = proxy
	legacy := map[string]string{"inbox": "categories-v1:" + proxy.URL + "/me/mailFolders/inbox/messages/delta?t&token=1", "archive": "categories-v1:" + proxy.URL + "/me/mailFolders/archive/messages/delta?t&token=1"}
	blob, err := json.Marshal(legacy)
	require.NoError(t, err)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(t, err)
	require.NoError(t, st.ScopedToSync(src.ID, run).CompleteSyncAndPreserveSourceCursorContext(t.Context(), run, src.ID, string(blob)))
	f.mu.Lock()
	f.version["m1"] = 1
	f.badValue["m1"] = true
	f.mu.Unlock()
	f.put("m1", "inbox")
	summary, err := f.sync(t, st)
	require.NoError(t, err)
	assert.True(t, summary.Incomplete, "a migration cannot discard a lost old stream's MIME obligation")
	last, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(t, err)
	var pending map[string]string
	require.NoError(t, json.Unmarshal([]byte(last.CursorAfter.String), &pending))
	assert.Contains(t, pending, "retry:m1")
	require.NoError(t, st.Close())
	reopened, err := store.Open(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	f.mu.Lock()
	delete(f.badValue, "m1")
	f.mu.Unlock()
	summary, err = f.sync(t, reopened)
	require.NoError(t, err)
	assert.False(t, summary.Incomplete)
	message, err := reopened.GetMessage(ids["m1"])
	require.NoError(t, err)
	assert.Equal(t, "body m1 v1", message.Snippet)
}

func TestProviderStateInterruptedMigrationMIMEWalkResumesContentRecovery(t *testing.T) {
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(t, err)
	src, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(t, err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(t, err)
	cursors := map[string]string{"inbox": "walk:migration-mime:" + f.srv.URL + "/me/mailFolders/inbox/messages/delta?walk"}
	blob, err := json.Marshal(cursors)
	require.NoError(t, err)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(t, err)
	require.NoError(t, st.FailSyncWithCheckpoint(run, "synthetic interrupted migration", &store.Checkpoint{PageToken: string(blob)}))
	f.mu.Lock()
	f.version["m1"] = 1
	f.mu.Unlock()
	summary, err := f.sync(t, st)
	require.NoError(t, err)
	assert.False(t, summary.Incomplete)
	message, err := st.GetMessage(ids["m1"])
	require.NoError(t, err)
	assert.Equal(t, "body m1 v1", message.Snippet)
}
