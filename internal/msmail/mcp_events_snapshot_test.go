package msmail

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func nativeMailSnapshotSyncer(t *testing.T, st *store.Store, f *fakeGraph) *syncer {
	t.Helper()
	source, err := st.GetOrCreateSource(SourceType, "me@example.com")
	require.NoError(t, err)
	_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(t, err)
	c := NewClient(f.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	s := &syncer{ingestMode: store.IngestLive, st: st.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: time.Now()}), c: c, sourceID: source.ID, opts: Options{Email: "me@example.com", AttachmentsDir: t.TempDir()}, log: slog.Default(), sum: &Summary{}, cursors: map[string]string{}}
	s.labels, err = s.ensureLabels(t.Context(), []Folder{{ID: "inbox", Path: "Inbox"}})
	require.NoError(t, err)
	return s
}

// These triggers reject actual mandatory SQL writes in the native download path.
func failMailSnapshotWrite(t *testing.T, st *store.Store, table string) func() {
	t.Helper()
	require := require.New(t)
	event := "INSERT"
	if table == "messages" {
		event = "UPDATE OF metadata"
	}
	name := "fail_mail_snapshot_" + table
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic snapshot failure'; END; $$`, name))
		require.NoError(err)
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, event, table, name))
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'synthetic snapshot failure'); END`, name, event, table))
		require.NoError(err)
	}
	release := func() {
		if st.IsPostgreSQL() {
			_, err := st.DB().Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
			require.NoError(err)
			_, err = st.DB().Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
			require.NoError(err)
		} else {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS ` + name)
			require.NoError(err)
		}
	}
	t.Cleanup(release)
	return release
}

func TestMCPMSMailSnapshotRollback(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "message_recipients", "attachments", "messages"} {
		t.Run(table, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			f := newFakeGraph(t)
			f.put("ready", "inbox")
			f.rawOverride = map[string]string{"ready": strings.Replace(rawWithAttachment("ready", false, ""), "Subject:", "In-Reply-To: <parent@example.com>\r\nSubject:", 1)}
			s := nativeMailSnapshotSyncer(t, st, f)
			release := failMailSnapshotWrite(t, st, table)
			_ = s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready"}})
			var messages, events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(messages, "a mandatory snapshot failure must leave no message")
			assert.Zero(events, "a mandatory snapshot failure must leave no occurrence")
			release()
			require.NoError(s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready"}}))
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(1, events, "the retry publishes the complete snapshot exactly once")
			var metadata string
			var refs int
			require.NoError(st.DB().QueryRow(`SELECT metadata FROM messages WHERE source_message_id='ready'`).Scan(&metadata))
			assert.Contains(metadata, "parent@example.com")
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&refs))
			assert.Equal(1, refs)
		})
	}
}

func TestMCPMSMailAttachmentPreparationFailureDoesNotPublish(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("ready", "inbox")
	f.withAttachment["ready"] = true
	s := nativeMailSnapshotSyncer(t, st, f)
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(blocker, nil, 0o600))
	s.opts.AttachmentsDir = filepath.Join(blocker, "attachments")
	_ = s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready"}})
	var messages, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(messages)
	assert.Zero(events)
	s.opts.AttachmentsDir = t.TempDir()
	require.NoError(s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready"}}))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(1, events)
}

func TestMCPMSMailRefreshRollbackPreservesSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("ready", "inbox")
	original := rawWithAttachment("ready", false, "")
	f.rawOverride = map[string]string{"ready": original}
	s := nativeMailSnapshotSyncer(t, st, f)
	require.NoError(s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready"}}))
	ids, err := st.MessageExistsBatch(s.sourceID, []string{"ready"})
	require.NoError(err)
	id := ids["ready"]
	require.NotZero(id)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET metadata=? WHERE id=?`), `{"synthetic_extension":true}`, id)
	require.NoError(err)
	f.rawOverride["ready"] = strings.Replace(rawWithAttachment("ready", true, "d29ybGQ="), "Subject:", "In-Reply-To: <parent@example.com>\r\nSubject:", 1)
	release := failMailSnapshotWrite(t, st, "messages")
	require.Error(s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready", archiveID: id}}))
	stored, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(original, string(stored))
	var key string
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_part_key FROM attachments WHERE message_id=?`), id).Scan(&key))
	assert.Equal("mime:2", key)
	release()
	require.NoError(s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready", archiveID: id}}))
	metadata, err := st.GetMessageMetadata(id)
	require.NoError(err)
	assert.JSONEq(`{"synthetic_extension":true,"email_in_reply_to":"parent@example.com"}`, metadata.String)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "refresh is not another arrival")
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_part_key FROM attachments WHERE message_id=?`), id).Scan(&key))
	assert.Equal("mime:3", key)
}

func TestMCPMSMailSnapshotWithoutAttachmentStorage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("ready", "inbox")
	f.withAttachment["ready"] = true
	s := nativeMailSnapshotSyncer(t, st, f)
	s.opts.AttachmentsDir = ""
	require.NoError(s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready"}}))
	ids, err := st.MessageExistsBatch(s.sourceID, []string{"ready"})
	require.NoError(err)
	raw, err := st.GetMessageRaw(ids["ready"])
	require.NoError(err)
	assert.Contains(string(raw), "aGVsbG8=")
	var refs, count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&refs))
	assert.Zero(refs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count)
}

func TestMCPMSMailSnapshotUsesEmailThread(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGraph(t)
	f.put("ready", "inbox")
	s := nativeMailSnapshotSyncer(t, st, f)
	require.NoError(s.download(t.Context(), "inbox", []DeltaMessage{{ID: "ready"}}))
	var conversationType string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_type FROM conversations WHERE id=(SELECT conversation_id FROM messages WHERE source_id=? AND source_message_id=?)`), s.sourceID, "ready").Scan(&conversationType))
	assert.Equal(t, "email_thread", conversationType, "native mail must retain email activity classification")
}

// A derived index failure must not strand a complete native MIME snapshot.
func TestMCPMSMailIndexFailureKeepsArchiveAndProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeMailEvents(t, st)
	f := newFakeGraph(t)
	_, err := f.sync(t, st)
	require.NoError(err)
	var dense strings.Builder
	for i := range 80000 {
		fmt.Fprintf(&dense, "w%d ", i)
	}
	body := dense.String()
	mime := "From: sender@example.com\r\nTo: me@example.com\r\nSubject: " + strings.ReplaceAll(body, "w", "s") + "\r\nMessage-ID: <dense@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
	if !st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TABLE messages_fts`)
		require.NoError(err)
	}
	f.put("dense", "inbox")
	f.put("healthy", "inbox")
	f.rawOverride = map[string]string{"dense": mime}
	_, err = f.sync(t, st)
	require.NoError(err, "an unavailable derived index must not block native sync")
	var id int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id='dense'`).Scan(&id))
	storedBody, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(sha256.Sum256([]byte(body)), sha256.Sum256([]byte(storedBody)))
	storedRaw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(sha256.Sum256([]byte(mime)), sha256.Sum256(storedRaw))
	assert.Equal(2, nativeMailEventCount(t, st))
	if st.IsPostgreSQL() {
		var unindexed bool
		require.NoError(st.DB().QueryRow(`SELECT search_fts IS NULL FROM messages WHERE source_message_id='dense'`).Scan(&unindexed))
		assert.True(unindexed, "fixture must exercise the native tsvector size limit")
	}
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(2, nativeMailEventCount(t, st))
	f.put("later", "inbox")
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(3, nativeMailEventCount(t, st))
}
