package msmail

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
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
			if !assert.NoError(t, json.Unmarshal(body, &payload)) {
				return
			}
			providerState, ok := state.Load().(map[string]any)
			if !assert.True(t, ok, "provider state fixture must be a map") {
				return
			}
			apply := func(item map[string]any) {
				maps.Copy(item, providerState)
			}
			if values, ok := payload["value"].([]any); ok {
				for _, value := range values {
					item, ok := value.(map[string]any)
					if !assert.True(t, ok, "Graph list values must be objects") {
						return
					}
					apply(item)
				}
			} else {
				apply(payload)
			}
			var err error
			body, err = json.Marshal(payload)
			if !assert.NoError(t, err) {
				return
			}
		}
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	f.srv = proxy // keep opaque delta links on the same fixture transport
	c := NewClient(proxy.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
	run := func() {
		t.Helper()
		_, err := Import(t.Context(), st, c, Options{Email: "sample@example.test", AttachmentsDir: t.TempDir()}, slog.Default())
		require.NoError(t, err)
	}
	assert := assert.New(t)
	require := require.New(t)
	run()
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "sample@example.test")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(err)
	labels := func() []string {
		t.Helper()
		rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT l.source_label_id FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_id=?`), ids["m1"])
		require.NoError(err)
		defer func() { _ = rows.Close() }()
		var result []string
		for rows.Next() {
			var id string
			require.NoError(rows.Scan(&id))
			result = append(result, id)
		}
		require.NoError(rows.Err())
		return result
	}
	assert.ElementsMatch([]string{"inbox", "msmail-state:unread", "msmail-state:flagged"}, labels())
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET is_read=TRUE WHERE id=?`), ids["m1"])
	require.NoError(err)
	state.Store(map[string]any{})
	f.put("m1", "archive")
	run()
	assert.ElementsMatch([]string{"archive", "msmail-state:unread", "msmail-state:flagged"}, labels())
	var localRead bool
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT is_read FROM messages WHERE id=?`), ids["m1"]).Scan(&localRead))
	assert.True(localRead, "provider unread state must not overwrite local reader state")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), ids["m1"])
	require.NoError(err)
	state.Store(map[string]any{"isRead": true, "flag": map[string]any{"flagStatus": "complete"}})
	f.put("m1", "archive")
	run()
	assert.Equal([]string{"archive"}, labels())
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT is_read FROM messages WHERE id=?`), ids["m1"]).Scan(&localRead))
	assert.False(localRead, "provider read state must not overwrite local reader state")
}

func TestProviderStateLegacyCursorRestartPreservesStateAndRetryDebt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "me@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(err)
	read, flag := false, "flagged"
	_, err = st.ReconcileMicrosoftMailStateContext(t.Context(), ids["m1"], nil, nil, store.MicrosoftMailState{IsRead: &read, FlagStatus: &flag})
	require.NoError(err)
	legacy := map[string]string{
		"inbox":    "categories-v1:" + f.srv.URL + "/me/mailFolders/inbox/messages/delta?t&token=1",
		"archive":  "categories-v1:" + f.srv.URL + "/me/mailFolders/archive/messages/delta?t&token=1",
		"retry:m1": "inbox",
	}
	blob, err := json.Marshal(legacy)
	require.NoError(err)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.ScopedToSync(src.ID, run).CompleteSyncAndPreserveSourceCursorContext(t.Context(), run, src.ID, string(blob)))
	require.NoError(st.Close())
	reopened, err := store.Open(path)
	require.NoError(err)
	defer func() { _ = reopened.Close() }()
	f.mu.Lock()
	f.categories = map[string][]string{"m1": {"Later"}}
	f.version["m1"] = 1
	f.mu.Unlock()
	f.put("m1", "archive")
	summary, err := f.sync(t, reopened)
	require.NoError(err, "old categories-v1 prefixes must be decoded before draining their URLs")
	assert.False(summary.Incomplete)
	message, err := reopened.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Archive", "Category: Later", "Provider: Unread", "Provider: Flagged"}, message.Labels)
	assert.Equal("body m1 v1", message.Snippet)
	last, err := reopened.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	var cursors map[string]string
	require.NoError(json.Unmarshal([]byte(last.CursorAfter.String), &cursors))
	assert.NotContains(cursors, "retry:m1")
	assert.True(strings.HasPrefix(cursors["inbox"], "provider-state-v2:"))
	assert.True(strings.HasPrefix(cursors["archive"], "provider-state-v2:"))
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
			assert := assert.New(t)
			require := require.New(t)
			defer proxy.Close()
			f.srv = proxy
			st := testutil.NewTestStore(t)
			summary, err := f.sync(t, st)
			require.NoError(err)
			assert.Equal(1, summary.Added, "duplicate observations identify one new message")
			assert.EqualValues(1, f.mimeCalls.Load(), "compatible partial observations need one MIME acquisition")
			src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "me@example.com")
			require.NoError(err)
			ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
			require.NoError(err)
			message, err := st.GetMessage(ids["m1"])
			require.NoError(err)
			assert.ElementsMatch([]string{"Inbox", "Category: Blue", "Provider: Unread", "Provider: Flagged"}, message.Labels)
		})
	}
}

func TestProviderStateUnknownDestinationRetainsDurableAcquisitionDebt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "me@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(err)
	read, flag := false, "flagged"
	_, err = st.ReconcileMicrosoftMailStateContext(t.Context(), ids["m1"], nil, nil, store.MicrosoftMailState{IsRead: &read, FlagStatus: &flag})
	require.NoError(err)
	f.mu.Lock()
	f.categories = map[string][]string{"m1": {"Moved"}}
	f.version["m1"] = 1
	f.mu.Unlock()
	f.put("m1", "later") // Lookup knows this destination before this run's folder list.
	summary, err := f.sync(t, st)
	require.NoError(err)
	assert.True(summary.Incomplete, "unresolved folder acquisition cannot report catchup")
	last, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	var cursors map[string]string
	require.NoError(json.Unmarshal([]byte(last.CursorAfter.String), &cursors))
	assert.Equal("later", cursors["retry:m1"])
	require.NoError(st.Close())
	reopened, err := store.Open(path)
	require.NoError(err)
	defer func() { _ = reopened.Close() }()
	f.mu.Lock()
	f.folders = append(f.folders, "later")
	f.names = map[string]string{"later": "Later"}
	f.mu.Unlock()
	summary, err = f.sync(t, reopened)
	require.NoError(err)
	assert.False(summary.Incomplete)
	message, err := reopened.GetMessage(ids["m1"])
	require.NoError(err)
	assert.ElementsMatch([]string{"Later", "Category: Moved", "Provider: Unread", "Provider: Flagged"}, message.Labels)
	assert.Equal("body m1 v1", message.Snippet)
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
	assert := assert.New(t)
	require := require.New(t)
	f.srv = proxy
	summary, err := f.sync(t, st)
	require.NoError(err)
	assert.True(summary.Incomplete)
	last, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	var pending map[string]string
	require.NoError(json.Unmarshal([]byte(last.CursorAfter.String), &pending))
	assert.Contains(pending, "retry:m1")
	require.NoError(st.Close())
	reopened, err := store.Open(path)
	require.NoError(err)
	defer func() { _ = reopened.Close() }()
	// The fresh importer must replay the existing durable intent, not depend on
	// an in-memory afterStore result or scan all historical MIME.
	active.Store(reopened)
	f.attachDir = t.TempDir()
	summary, err = f.sync(t, reopened)
	require.NoError(err)
	assert.False(summary.Incomplete)
	var attachments int
	require.NoError(reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM attachments`).Scan(&attachments))
	assert.Equal(1, attachments)
}

func TestProviderStateMigrationLostOldStreamRetainsMIMEDebt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	f.put("m2", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "me@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(err)
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
		maps.Copy(w.Header(), recorder.Header())
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer proxy.Close()
	f.srv = proxy
	legacy := map[string]string{"inbox": "categories-v1:" + proxy.URL + "/me/mailFolders/inbox/messages/delta?t&token=1", "archive": "categories-v1:" + proxy.URL + "/me/mailFolders/archive/messages/delta?t&token=1"}
	blob, err := json.Marshal(legacy)
	require.NoError(err)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.ScopedToSync(src.ID, run).CompleteSyncAndPreserveSourceCursorContext(t.Context(), run, src.ID, string(blob)))
	f.mu.Lock()
	f.version["m1"] = 1
	f.badValue["m1"] = true
	f.mu.Unlock()
	f.put("m1", "inbox")
	f.pageSize = 1
	f.stopAt = 1
	f.stopStatus = http.StatusBadRequest
	_, err = f.sync(t, st)
	require.Error(err, "the enrichment walk is interrupted after the old stream was proven live")
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	require.NotNil(checkpoint)
	var interrupted map[string]string
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &interrupted))
	assert.Equal(migrationStreamPrefix+strings.TrimPrefix(legacy["inbox"], categoryDeltaPrefix), interrupted["inbox"], "the live-stream phase survives the interrupted enrichment walk")
	require.NoError(st.Close())
	reopened, err := store.Open(path)
	require.NoError(err)
	defer func() { _ = reopened.Close() }()
	summary, err := f.sync(t, reopened)
	require.NoError(err)
	assert.True(summary.Incomplete, "a migration cannot discard a lost old stream's MIME obligation")
	last, err := reopened.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	var pending map[string]string
	require.NoError(json.Unmarshal([]byte(last.CursorAfter.String), &pending))
	assert.Contains(pending, "retry:m1")
	f.mu.Lock()
	delete(f.badValue, "m1")
	f.mu.Unlock()
	summary, err = f.sync(t, reopened)
	require.NoError(err)
	assert.False(summary.Incomplete)
	message, err := reopened.GetMessage(ids["m1"])
	require.NoError(err)
	assert.Equal("body m1 v1", message.Snippet)
}

func TestProviderStateInterruptedMigrationMIMEWalkResumesContentRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("m1", "inbox")
	_, err := f.sync(t, st)
	require.NoError(err)
	src, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "me@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m1"})
	require.NoError(err)
	cursors := map[string]string{"inbox": "walk:migration-mime:" + f.srv.URL + "/me/mailFolders/inbox/messages/delta?walk"}
	blob, err := json.Marshal(cursors)
	require.NoError(err)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.FailSyncWithCheckpoint(run, "synthetic interrupted migration", &store.Checkpoint{PageToken: string(blob)}))
	f.mu.Lock()
	f.version["m1"] = 1
	f.mu.Unlock()
	summary, err := f.sync(t, st)
	require.NoError(err)
	assert.False(summary.Incomplete)
	message, err := st.GetMessage(ids["m1"])
	require.NoError(err)
	assert.Equal("body m1 v1", message.Snippet)
}
