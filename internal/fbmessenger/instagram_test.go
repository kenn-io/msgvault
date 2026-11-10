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
		limit                              int
		unsupported                        bool
	}{
		{name: "empty account", selfName: "Nova Example", wantError: "account"},
		{name: "blank normalized account", account: "@", selfName: "Nova Example", wantError: "account"},
		{name: "empty self name", account: "example", wantError: "self-name"},
		{name: "blank self name", account: "example", selfName: "   ", wantError: "self-name"},
		{name: "negative limit", account: "example", selfName: "Nova Example", limit: -1, wantError: "--limit must be non-negative"},
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
			_, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: test.account, SelfName: test.selfName, Limit: test.limit})
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
	ids := instagramIDs(t, st)
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
	assert.Equal(ids, instagramIDs(t, st))
	var title, kind string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title,conversation_type FROM conversations`).Scan(&title, &kind))
	assert.Equal("Changed fictional title", title)
	assert.Equal("group_chat", kind)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Changed"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(2), total)
	var recipients int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(DISTINCT mr.message_id) FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE p.display_name = 'Lumen Example'`).Scan(&recipients))
	assert.Equal(2, recipients)
}

func TestInstagramSharedLinkDecoded(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	record := instagramTestRecord("")
	record["share"] = map[string]string{"link": "https://example.com/cafÃ©"}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	_, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
	require.NoError(t, err)
	var body string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Equal(t, "[shared link] https://example.com/café", body)
	t.Run("search", func(t *testing.T) {
		testutil.SkipIfPostgres(t, "PostgreSQL tokenizes URL paths separately from plain text")
		_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"café"}}, 0, 10)
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
	})
}

func TestInstagramRejectsNewerFingerprintInAbsentThread(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	old := instagramTestShard(t, root, "old", "message_1.json", instagramTestRecord("original"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	require.NoError(t, os.Remove(old))
	instagramTestShard(t, root, "new", "message_1.json", instagramTestRecord("new"))
	for _, version := range []string{"ig:v2:", "ig:v10:"} {
		_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET source_message_id = ? || substr(source_message_id, 7)`, version)
		require.NoError(t, err)
		_, err = ImportDYI(t.Context(), st, opts)
		require.ErrorContains(t, err, "unsupported fingerprint version")
		assert.Equal(t, 1, countMessages(t, st, ""))
	}
}

func TestInstagramAddsOnlySurplusCopiesWithSharedReactions(t *testing.T) {
	for name, shared := range map[string]bool{"distinct": false, "shared": true} {
		t.Run(name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			copyOf := func(reactions ...string) map[string]any {
				record := instagramTestRecord("same")
				var list []map[string]string
				for _, reaction := range reactions {
					list = append(list, map[string]string{"actor": "Nova Example", "reaction": reaction})
				}
				record["reactions"] = list
				return record
			}
			instagramTestShard(t, root, "circle", "message_1.json", copyOf("a"), copyOf("b"))
			opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(t, err)
			original := instagramIDs(t, st)
			extra := []string{}
			if shared {
				extra = []string{"x"}
			}
			instagramTestShard(t, root, "circle", "message_1.json", copyOf(append([]string{"d"}, extra...)...), copyOf(append([]string{"e"}, extra...)...), copyOf(append([]string{"f"}, extra...)...))
			summary, err := ImportDYI(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, int64(1), summary.MessagesAdded)
			assert.True(t, summary.Incomplete)
			assert.Equal(t, 3, countMessages(t, st, ""))
			var count int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('a','b')`).Scan(&count))
			assert.Equal(t, 2, count)
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('d','e','f')`).Scan(&count))
			assert.Zero(t, count)
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value = 'x'`).Scan(&count))
			assert.Equal(t, len(extra), count)
			assert.Subset(t, instagramIDs(t, st), original)
			summary, err = ImportDYI(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Zero(t, summary.MessagesAdded)
			assert.Equal(t, 3, countMessages(t, st, ""))
		})
	}
}

func TestInstagramDistinctCallContent(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	call := instagramTestRecord("")
	call["type"] = "Call"
	call["call_duration"] = 60
	instagramTestShard(t, root, "circle", "message_1.json", call)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	call["call_duration"] = 120
	instagramTestShard(t, root, "circle", "message_1.json", call)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Equal(t, 2, countMessages(t, st, ""))
	assert.Equal(t, 1, countMessages(t, st, "snippet='[call: 1m 0s]'"))
	assert.Equal(t, 1, countMessages(t, st, "snippet='[call: 2m 0s]'"))
	call["missed"] = true
	instagramTestShard(t, root, "circle", "message_1.json", call)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Equal(t, 3, countMessages(t, st, ""))
	call["type"] = "Unsubscribe"
	instagramTestShard(t, root, "circle", "message_1.json", call)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Equal(t, 4, countMessages(t, st, ""))
}

func TestInstagramReactionRemoval(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	record := instagramTestRecord("hello")
	record["reactions"] = []map[string]string{{"actor": "Reactiononly", "reaction": "✨"}}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Reactiononly"}}, 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	delete(record, "reactions")
	instagramTestShard(t, root, "circle", "message_1.json", record)
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&n))
	assert.Zero(t, n)
	assert.Equal(t, 1, countMessages(t, st, "snippet='hello'"))
	_, total, err = st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Reactiononly"}}, 0, 10)
	require.NoError(t, err)
	assert.Zero(t, total)
}

func TestInstagramExcludedCandidates(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("hello"))
	invalid := filepath.Join(root, filepath.FromSlash(instagramInbox), "invalid")
	require.NoError(t, os.MkdirAll(invalid, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(invalid, "message_final.json"), []byte(`{}`), 0600))
	requests := filepath.Join(root, "your_instagram_activity", "messages", "message_requests", "request")
	require.NoError(t, os.MkdirAll(requests, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(requests, "message_1.json"), []byte(`{}`), 0600))
	summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
	require.NoError(t, err)
	assert.True(t, summary.Incomplete)
	assert.Equal(t, int64(2), summary.FilesSkipped)
}

func TestInstagramRescanNewRootAfterLimit(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "later", "message_1.json", instagramTestRecord("later"), instagramTestRecord("second"))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example", Limit: 1}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	newRoot := t.TempDir()
	require.NoError(t, os.CopyFS(newRoot, os.DirFS(root)))
	instagramTestShard(t, newRoot, "earlier", "message_1.json", instagramTestRecord("earlier"))
	opts.RootDir, opts.Limit = newRoot, 0
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 3, countMessages(t, st, ""))
}

func TestInstagramSenderReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name, sender  string
		rename, media bool
		added         int64
	}{
		{name: "rename", sender: "Renamed Example", rename: true},
		{name: "different sender", sender: "Lumen Example", added: 1},
		{name: "different media", sender: "Renamed Example", rename: true, media: true, added: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			record := instagramTestRecord("hello")
			if tc.media {
				record["photos"] = []map[string]string{{"uri": "photos/one.jpg"}}
			}
			filename := instagramTestShard(t, root, "circle", "message_1.json", record)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(t, err)
			ids := instagramIDs(t, st)
			record["sender_name"] = tc.sender
			if tc.media {
				record["photos"] = []map[string]string{{"uri": "photos/two.jpg"}}
			}
			data, err := os.ReadFile(filename)
			require.NoError(t, err)
			var shard map[string]any
			require.NoError(t, json.Unmarshal(data, &shard))
			shard["messages"] = []map[string]any{record}
			if tc.rename {
				shard["participants"] = []map[string]string{{"name": "Nova Example"}, {"name": tc.sender}}
			}
			data, err = json.Marshal(shard)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filename, data, 0600))
			summary, err := ImportDYI(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.added, summary.MessagesAdded)
			if tc.added > 0 {
				assert.Equal(t, 2, countMessages(t, st, ""))
				return
			}
			assert.Equal(t, ids, instagramIDs(t, st))
			summary, err = ImportDYI(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Zero(t, summary.MessagesAdded)
			assert.Equal(t, ids, instagramIDs(t, st))
		})
	}
}

func TestInstagramDuplicateOccurrences(t *testing.T) {
	t.Run("reordered", func(t *testing.T) {
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
		var created sql.NullTime
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT created_at FROM reactions LIMIT 1`).Scan(&created))
		assert.False(t, created.Valid)
		require.NoError(t, os.Remove(filepath.Join(root, filepath.FromSlash(instagramInbox), "circle", "message_2.json")))
		original := instagramIDs(t, st)
		instagramTestShard(t, root, "circle", "message_1.json", b)
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(t, err)
		assert.Zero(t, summary.MessagesAdded)
		assert.Equal(t, original, instagramIDs(t, st))
		c := instagramTestRecord("hello")
		c["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "c"}}
		instagramTestShard(t, root, "circle", "message_1.json", a, c)
		summary, err = ImportDYI(t.Context(), st, opts)
		require.NoError(t, err)
		assert.Zero(t, summary.MessagesAdded)
		assert.Equal(t, original, instagramIDs(t, st))

		beforeChanged := reactions()
		d := instagramTestRecord("hello")
		d["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "d"}}
		e := instagramTestRecord("hello")
		e["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "e"}}
		instagramTestShard(t, root, "circle", "message_1.json", d, e)
		summary, err = ImportDYI(t.Context(), st, opts)
		require.NoError(t, err)
		assert.Zero(t, summary.MessagesAdded)
		assert.Equal(t, original, instagramIDs(t, st))
		assert.True(t, summary.Incomplete)
		assert.Equal(t, beforeChanged, reactions())
		summary, err = ImportDYI(t.Context(), st, opts)
		require.NoError(t, err)
		assert.Zero(t, summary.MessagesAdded)
	})

	t.Run("unreadable evidence", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("same"))
		opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(t, err)
		_, err = st.DB().ExecContext(t.Context(), `DELETE FROM message_raw`)
		require.NoError(t, err)
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(t, err)
		assert.Equal(t, int64(1), summary.MessagesAdded)
		assert.Equal(t, 2, countMessages(t, st, ""))
	})
}
