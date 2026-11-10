package fbmessenger

import (
	bytespkg "bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInstagramImportReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	assert.Equal("39151bedba35d941336e56ea21fd767503efe5c6f0ddc63dcfd58ffaaa600733", instagramFingerprint(rawJSONMessage{SenderName: "Nova <Example>", Content: "&\u2028\u2029"}))
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Source: "instagram", RootDir: "testdata/instagram", Account: "orbit.example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()}
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(int64(6), summary.MessagesAdded)
	run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
	require.NoError(err)
	assert.Equal(summary.MessagesProcessed, run.MessagesProcessed)
	assert.Equal(summary.MessagesAdded, run.MessagesAdded)
	assert.Empty(run.CursorBefore.String)

	assert.Equal(2, summary.ThreadsProcessed)
	assert.Equal(1, summary.MediaMissing)
	assert.Equal(1, countMessages(t, st, "is_from_me = TRUE"))
	beforeRevision, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	afterRevision, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(beforeRevision, afterRevision)
	assert.Equal(6, countMessages(t, st, "message_type='instagram'"))
	results, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"telescope"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	assert.Len(results, 1)
	var timestamp time.Time
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sent_at FROM messages WHERE snippet='The telescope is ready.'`).Scan(&timestamp))
	assert.Equal(int64(1730500000123), timestamp.UnixMilli())
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE snippet LIKE 'café%'`).Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Contains(string(raw), "message_1.json")
	assert.Contains(string(raw), "timestamp_ms")
	assert.Contains(string(raw), `caf\u00c3\u00a9`)
	ids := instagramIDs(t, st)
	assert.Contains(ids, "ig:v1:9eb6fb43c374a0f75d8413e28359697c136b8c31492b361d95a71f2d60998e6e:f381d2c678356978d8dd328d259fb5c61f79f27b7cfd885a661ba280c8416ef0:0")
	assert.Contains(ids, "ig:v1:9eb6fb43c374a0f75d8413e28359697c136b8c31492b361d95a71f2d60998e6e:3225a563445b0a8cb0a916f3543b430e6a707b78759cc7c507f39c50846472cb:0")
	before := instagramIDs(t, st)
	relocated := t.TempDir()
	require.NoError(os.CopyFS(relocated, os.DirFS("testdata/instagram")))
	filename := filepath.Join(relocated, filepath.FromSlash(instagramInbox), "mira_101", "message_1.json")
	data, err := os.ReadFile(filename)
	require.NoError(err)
	var shard map[string]any
	require.NoError(json.Unmarshal(data, &shard))
	shard["title"] = "Renamed fictional conversation"
	data, err = json.Marshal(shard)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, data, 0600))
	opts.RootDir = relocated
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(before, instagramIDs(t, st))
}

func instagramTestShard(t *testing.T, root, thread, shard string, records ...map[string]any) string {
	t.Helper()
	require := require.New(t)
	filename := filepath.Join(root, filepath.FromSlash(instagramInbox), thread, shard)
	require.NoError(os.MkdirAll(filepath.Dir(filename), 0700))
	data, err := json.Marshal(map[string]any{"thread_path": "inbox/" + thread, "title": "Fictional circle", "participants": []map[string]string{{"name": "Nova Example"}, {"name": "Mira Example"}}, "messages": records})
	require.NoError(err)
	require.NoError(os.WriteFile(filename, data, 0600))
	return filename
}
func instagramTestRecord(text string) map[string]any {
	return map[string]any{"sender_name": "Mira Example", "timestamp_ms": int64(1730500000123), "content": text}
}
func instagramIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	require := require.New(t)
	rows, err := st.DB().QueryContext(t.Context(), `SELECT source_message_id FROM messages ORDER BY source_message_id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(rows.Err())
	return ids
}

func TestInstagramRepartitionMultiplicityAndReactions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	a, b := instagramTestRecord("amber"), instagramTestRecord("violet")
	a["photos"] = []map[string]string{{"uri": "photos/missing.png"}, {"uri": "photos/missing.png"}}
	first := instagramTestShard(t, root, "circle", "message_1.json", a, a, b)
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(int64(3), summary.MessagesAdded)
	// Duplicate references share one attachment slot per retained message.
	checkAttachmentStats := func(want int) {
		t.Helper()
		var rows, mismatches int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&rows))
		assert.Equal(want, rows)
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages m WHERE m.attachment_count <> (SELECT count(*) FROM attachments a WHERE a.message_id = m.id) OR m.has_attachments <> EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id)`).Scan(&mismatches))
		assert.Zero(mismatches)
	}
	checkAttachmentStats(2)
	a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "✨"}}
	instagramTestShard(t, root, "circle", "message_1.json", b, a)
	instagramTestShard(t, root, "circle", "message_2.json", a)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(int64(2), summary.MessagesAdded)
	checkAttachmentStats(4)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&n))
	assert.Equal(2, n)
	var created sql.NullTime
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT created_at FROM reactions LIMIT 1`).Scan(&created))
	assert.False(created.Valid)
	// Changed body is new evidence without an upstream ID; absence retains old rows.
	require.NoError(os.Remove(first))
	instagramTestShard(t, root, "circle", "message_2.json", instagramTestRecord("edited amber"))
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(6, countMessages(t, st, ""))
}

func TestInstagramAccountAndExactNameIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	a := instagramTestRecord("same person in two chats")
	instagramTestShard(t, root, "dm", "message_1.json", a)
	filename := instagramTestShard(t, root, "group", "message_1.json", a)
	var shard map[string]any
	bytes, err := os.ReadFile(filename)
	require.NoError(err)
	require.NoError(json.Unmarshal(bytes, &shard))
	shard["participants"] = []map[string]string{{"name": "Nova Example"}, {"name": "Mira Example"}, {"name": "Mira.Example"}}
	bytes, err = json.Marshal(shard)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, bytes, 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "@EXAMPLE", SelfName: "mira example"}
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(countMessages(t, st, "is_from_me=TRUE"))
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM participants WHERE display_name='Mira Example'`).Scan(&n))
	assert.Equal(1, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM participants WHERE display_name='Mira.Example'`).Scan(&n))
	assert.Equal(1, n)
	opts.Account = "example"
	opts.SelfName = "Mira Example"
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(2, countMessages(t, st, "is_from_me=TRUE"))
	opts.Account = "another.example"
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(4, countMessages(t, st, ""))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sources WHERE source_type='instagram_messages'`).Scan(&n))
	assert.Equal(2, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM participants WHERE display_name='Mira Example'`).Scan(&n))
	assert.Equal(2, n)
}

func TestInstagramLimitReplayAndMissingValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	missingTime := map[string]any{"content": "no timestamp"}
	unknownSender := map[string]any{"timestamp_ms": 0, "content": "epoch unknown sender"}
	corrupt := map[string]any{"timestamp_ms": "yesterday", "content": "invalid time"}
	instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("one"), instagramTestRecord("two"), instagramTestRecord("three"), missingTime, unknownSender, corrupt)
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example", Limit: 1}
	for i := 1; i <= 3; i++ {
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.True(summary.LimitReached)
		run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
		require.NoError(err)
		assert.Equal(summary.MessagesProcessed, run.MessagesProcessed)
		assert.Equal(store.SyncStatusPaused, run.Status)
		assert.Equal(int64(2), run.ErrorsCount)
		assert.Equal(int64(1), run.MessagesAdded)
		assert.Empty(run.CursorBefore.String)

		assert.Equal(int64(1), summary.MessagesAdded)
		assert.Equal(i, countMessages(t, st, ""))
	}
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.False(summary.LimitReached)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.True(summary.Incomplete)
	var sender sql.NullInt64
	var stamp time.Time
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id,sent_at FROM messages WHERE snippet='epoch unknown sender'`).Scan(&sender, &stamp))
	assert.False(sender.Valid)
	assert.Equal(time.UnixMilli(0).UTC(), stamp.UTC())
}

func TestInstagramMediaRecoveryAndContainment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	media := "your_instagram_activity/messages/inbox/circle/photos/fiction.txt"
	record := instagramTestRecord("media")
	record["photos"] = []map[string]string{{"uri": media}, {"uri": "../escape.txt"}, {"uri": "/absolute.txt"}}
	require.NoError(os.WriteFile(filepath.Join(root, "voice.ogg"), []byte("invented audio payload"), 0600))
	record["audio_files"] = []map[string]string{{"uri": "voice.ogg"}}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example", AttachmentsDir: t.TempDir()}
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.MediaMissing)
	assert.Equal(2, summary.MediaRejected)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&n))
	assert.Equal(4, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments WHERE COALESCE(content_hash, '') <> ''`).Scan(&n))
	assert.Equal(1, n)
	var mimeType string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT mime_type FROM attachments WHERE filename = 'voice.ogg'`).Scan(&mimeType))
	assert.Equal("audio/ogg", mimeType)
	full := filepath.Join(root, filepath.FromSlash(media))
	require.NoError(os.MkdirAll(filepath.Dir(full), 0700))
	require.NoError(os.WriteFile(full, []byte("invented star chart"), 0600))
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(2, summary.AttachmentsStored)
	require.NoError(os.Remove(full))
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments WHERE storage_path <> ''`).Scan(&n))
	assert.Equal(2, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&n))
	assert.Equal(4, n)
}

func TestInstagramConflictingThreadPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("retained"))
	filename := instagramTestShard(t, root, "circle", "message_2.json", instagramTestRecord("conflict"))
	bytes, err := os.ReadFile(filename)
	require.NoError(err)
	bytes = bytespkg.Replace(bytes, []byte(`"thread_path":"inbox/circle"`), []byte(`"thread_path":"inbox/other"`), 1)
	require.NoError(os.WriteFile(filename, bytes, 0600))
	_, err = ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"})
	require.ErrorContains(err, "conflicting thread_path")
	assert.Zero(countMessages(t, st, ""))
}

func TestInstagramRejectsBeforeSource(t *testing.T) {
	for _, test := range []struct {
		name, account, selfName, wantError string
		unsupported                        bool
	}{
		{name: "empty account", selfName: "Nova Example", wantError: "account"},
		{name: "blank normalized account", account: "@", selfName: "Nova Example", wantError: "account"},
		{name: "empty self name", account: "example", wantError: "self-name"},
		{name: "blank self name", account: "example", selfName: "   ", wantError: "self-name"},
		{name: "unsupported layout", account: "example", selfName: "Nova Example", wantError: "no supported inbox", unsupported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := "testdata/instagram"
			if test.unsupported {
				root = t.TempDir()
				require.NoError(os.WriteFile(filepath.Join(root, "messages.html"), []byte("fictional HTML"), 0600))
			}
			_, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: test.account, SelfName: test.selfName})
			require.ErrorContains(err, test.wantError)
			var n int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sources`).Scan(&n))
			assert.Zero(n)
		})
	}
}

func TestInstagramCanceledAfterCommitReplaysAncillaryWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	record := instagramTestRecord("cancel after commit")
	record["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "✨"}}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&n))
		return n > 0
	})
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	summary, err := ImportDYI(ctx, st, opts)
	require.ErrorIs(err, context.Canceled)
	assert.True(canceled())
	run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
	require.NoError(err)
	assert.Equal(summary.MessagesProcessed, run.MessagesProcessed)
	assert.Equal(int64(1), run.MessagesAdded)
	assert.Empty(run.CursorBefore.String)

	assert.Equal(int64(1), summary.MessagesAdded)
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal("failed", status)
	var counted int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(SUM(message_count), 0) FROM conversations`).Scan(&counted))
	assert.Equal(1, counted)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(1, summary.ReactionsAdded)
}

// The standard-library fuzz runner checks fingerprint invariants without adding
// a dependency. Seed runs are also real parser/identity checks under go test.
func FuzzInstagramFingerprint(f *testing.F) {
	f.Add("Mira Example", "starlight", int64(1730500000123))
	f.Add("Zoë Example", "cafÃ©", int64(0))
	f.Add("", "🌙", int64(-1))
	f.Fuzz(func(t *testing.T, sender, content string, stamp int64) {
		assert := assert.New(t)
		a := rawJSONMessage{SenderName: sender, Content: content, TimestampMs: stamp, Photos: []rawJSONPhoto{{URI: "photos/amber.jpg"}, {URI: "photos/violet.jpg"}}}
		b := a
		b.Photos = []rawJSONPhoto{{URI: "photos/violet.jpg"}, {URI: "photos/amber.jpg"}}
		b.Reactions = []rawJSONReaction{{Actor: "Nova Example", Reaction: "✨"}}
		assert.Equal(instagramFingerprint(a), instagramFingerprint(b))
		b.TimestampMs = stamp ^ 1
		assert.NotEqual(instagramFingerprint(a), instagramFingerprint(b))
	})
}

func TestInstagramGenericMediaAndThreadUpdates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	record := instagramTestRecord("generic media")
	record["media"] = []map[string]string{{"uri": "invented.txt"}}
	filename := instagramTestShard(t, root, "circle", "message_1.json", record, instagramTestRecord("second"))
	require.NoError(os.WriteFile(filepath.Join(root, "invented.txt"), []byte("invented media bytes"), 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()}
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	rows, err := st.DB().QueryContext(t.Context(), `SELECT id FROM messages ORDER BY id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(rows.Err())
	require.Len(ids, 2)
	for _, id := range ids {
		raw, err := st.GetMessageRawContext(t.Context(), id)
		require.NoError(err)
		var envelope map[string]json.RawMessage
		require.NoError(json.Unmarshal(raw, &envelope))
		assert.NotEmpty(envelope["record"])
	}
	data, err := os.ReadFile(filename)
	require.NoError(err)
	var shard map[string]any
	require.NoError(json.Unmarshal(data, &shard))
	shard["title"] = "Changed fictional title"
	shard["participants"] = []map[string]string{{"name": "Mira Example"}, {"name": "Nova Example"}, {"name": "Lumen Example"}}
	data, err = json.Marshal(shard)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, data, 0600))
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var title, kind string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title,conversation_type FROM conversations`).Scan(&title, &kind))
	assert.Equal("Changed fictional title", title)
	assert.Equal("group_chat", kind)
}
func TestInstagramReorderedDuplicateReactionsStayWithOccurrence(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	a, b := instagramTestRecord("hello"), instagramTestRecord("hello")
	a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "✨"}}
	b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "❤️"}}
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	reactions := func() map[string]string {
		rows, err := st.DB().QueryContext(t.Context(), `SELECT m.source_message_id, r.reaction_value FROM messages m JOIN reactions r ON r.message_id = m.id`)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		result := map[string]string{}
		for rows.Next() {
			var id, reaction string
			require.NoError(t, rows.Scan(&id, &reaction))
			assert.NotContains(t, result, id)
			result[id] = reaction
		}
		require.NoError(t, rows.Err())
		return result
	}
	before := reactions()
	instagramTestShard(t, root, "circle", "message_1.json", b)
	instagramTestShard(t, root, "circle", "message_2.json", a)
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&count))
	assert.Equal(t, 2, count)
	assert.Equal(t, before, reactions())
}

func TestInstagramRejectsNewerFingerprintInAbsentThread(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	old := instagramTestShard(t, root, "old", "message_1.json", instagramTestRecord("original"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET source_message_id = REPLACE(source_message_id, 'ig:v1:', 'ig:v2:')`)
	require.NoError(t, err)
	require.NoError(t, os.Remove(old))
	instagramTestShard(t, root, "new", "message_1.json", instagramTestRecord("new"))
	_, err = ImportDYI(t.Context(), st, opts)
	require.ErrorContains(t, err, "unsupported fingerprint version")
	assert.Equal(t, 1, countMessages(t, st, ""))
}

func TestInstagramReconcilesChangedAndMissingOccurrences(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	a, b := instagramTestRecord("same"), instagramTestRecord("same")
	a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "a"}}
	b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "b"}}
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	original := instagramIDs(t, st)
	instagramTestShard(t, root, "circle", "message_1.json", b)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, summary.MessagesAdded)
	assert.Equal(t, original, instagramIDs(t, st))
	c := instagramTestRecord("same")
	c["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "c"}}
	instagramTestShard(t, root, "circle", "message_1.json", a, c)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, summary.MessagesAdded)
	assert.Equal(t, original, instagramIDs(t, st))
	d := instagramTestRecord("same")
	d["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "d"}}
	e := instagramTestRecord("same")
	e["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "e"}}
	instagramTestShard(t, root, "circle", "message_1.json", d, e)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.MessagesAdded)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, summary.MessagesAdded)
}
