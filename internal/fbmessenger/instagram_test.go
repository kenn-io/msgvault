package fbmessenger

import (
	bytespkg "bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

func TestInstagramEmptyMediaURI(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	record := instagramTestRecord("message with incomplete media")
	for _, key := range []string{"photos", "videos", "audio_files", "files", "media", "gifs"} {
		record[key] = []map[string]string{{"uri": ""}}
	}
	record["sticker"] = map[string]string{"uri": ""}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
	require.NoError(t, err)
	assert.True(t, summary.Incomplete)
	assert.Equal(t, 7, summary.MediaRejected)
	assert.Equal(t, 1, countMessages(t, st, ""))
}

func TestInstagramDuplicateMediaSlot(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("photo"), 0600))
	record := instagramTestRecord("duplicate photo")
	record["photos"] = []map[string]string{{"uri": "photo.jpg"}, {"uri": "photo.jpg"}}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsStored)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestInstagramMediaRecoveryAndContainment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	media := "your_instagram_activity/messages/inbox/circle/photos/fiction.txt"
	record := instagramTestRecord("media")
	photos := []map[string]string{{"uri": media}, {"uri": "../escape.txt"}, {"uri": "/absolute.txt"}, {"uri": "directory"}}
	require.NoError(os.Mkdir(filepath.Join(root, "directory"), 0700))
	require.NoError(os.WriteFile(filepath.Join(root, "voice.ogg"), []byte("invented audio payload"), 0600))
	rejected, attachmentCount := 3, 5
	if os.Symlink(filepath.Join(root, "voice.ogg"), filepath.Join(root, "linked.jpg")) == nil {
		photos = append(photos, map[string]string{"uri": "linked.jpg"})
		rejected, attachmentCount = rejected+1, attachmentCount+1
	}
	record["photos"] = photos
	record["audio_files"] = []map[string]string{{"uri": "voice.ogg"}}
	instagramTestShard(t, root, "circle", "message_1.json", record, instagramTestRecord("later"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example", AttachmentsDir: t.TempDir()}
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.MediaMissing)
	assert.Equal(rejected, summary.MediaRejected)
	assert.Equal(2, countMessages(t, st, ""))
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&n))
	assert.Equal(attachmentCount, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments WHERE COALESCE(content_hash, '') <> ''`).Scan(&n))
	assert.Equal(1, n)
	full := filepath.Join(root, filepath.FromSlash(media))
	require.NoError(os.MkdirAll(filepath.Dir(full), 0700))
	require.NoError(os.WriteFile(full, []byte("invented star chart"), 0600))
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(rejected, summary.MediaRejected)
	assert.Equal(1, summary.AttachmentsStored)
	require.NoError(os.Remove(full))
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments WHERE storage_path <> ''`).Scan(&n))
	assert.Equal(2, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&n))
	assert.Equal(attachmentCount, n)
}

func TestInstagramReplayPreservesReplacedMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	media := filepath.Join(root, "photo.jpg")
	require.NoError(os.WriteFile(media, []byte("first image"), 0600))
	record := instagramTestRecord("photo")
	record["photos"] = []map[string]string{{"uri": "photo.jpg"}}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example", AttachmentsDir: t.TempDir()}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var original, storedPath string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT content_hash, storage_path FROM attachments`).Scan(&original, &storedPath))
	require.NotEmpty(original)
	require.NoError(os.WriteFile(media, []byte("different image"), 0600))
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Zero(summary.AttachmentsStored)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM attachments WHERE content_hash = ?`), original).Scan(&count))
	assert.Equal(1, count, "original media must remain attached to the message")
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, filepath.FromSlash(storedPath)))
	require.NoError(err)
	assert.Equal("first image", string(data))
	require.NoError(os.Remove(media))
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MediaMissing)
	assert.False(summary.Incomplete)
	data, err = os.ReadFile(filepath.Join(opts.AttachmentsDir, filepath.FromSlash(storedPath)))
	require.NoError(err)
	assert.Equal("first image", string(data))
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
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(store.SyncStatusFailed, run.Status)
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
	_, err = ImportDYI(t.Context(), st, opts)
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

func TestInstagramConversationUsesFirstShardHeader(t *testing.T) {
	t.Run("header and raw provenance", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		first := instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("new"))
		second := instagramTestShard(t, root, "circle", "message_2.json", instagramTestRecord("old"))
		writeHeader := func(filename, title string, participants []map[string]string) {
			data, err := os.ReadFile(filename)
			require.NoError(err)
			var shard map[string]any
			require.NoError(json.Unmarshal(data, &shard))
			shard["title"], shard["participants"] = title, participants
			data, err = json.Marshal(shard)
			require.NoError(err)
			require.NoError(os.WriteFile(filename, data, 0600))
		}
		writeHeader(first, "New title", []map[string]string{{"name": "Nova Example"}, {"name": "Mira Example"}, {"name": "Lumen Example"}})
		writeHeader(second, "Old title", []map[string]string{{"name": "Nova Example"}, {"name": "Mira Example"}})
		opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
		for range 2 {
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			var title, kind string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title, conversation_type FROM conversations`).Scan(&title, &kind))
			assert.Equal("New title", title)
			assert.Equal("group_chat", kind)
			_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"New"}}, 0, 10)
			require.NoError(err)
			assert.Equal(int64(2), total)
			var recipients int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(DISTINCT mr.message_id) FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE p.display_name='Lumen Example'`).Scan(&recipients))
			assert.Equal(2, recipients)
		}
		var oldID int64
		ids := instagramIDs(t, st)
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE snippet='old'`).Scan(&oldID))
		raw, err := st.GetMessageRawContext(t.Context(), oldID)
		require.NoError(err)
		assert.Contains(string(raw), "Old title")
		require.NoError(os.WriteFile(first, []byte(`{"thread_path":"inbox/circle","title":"Latest title","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[]}`), 0600))
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Equal(ids, instagramIDs(t, st))
		var title, kind string
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title, conversation_type FROM conversations`).Scan(&title, &kind))
		assert.Equal("Latest title", title)
		assert.Equal("direct_chat", kind)
		_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Latest"}}, 0, 10)
		require.NoError(err)
		assert.Equal(int64(1), total)
	})
	t.Run("empty first shard type change", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		first := instagramTestShard(t, root, "circle", "message_1.json")
		require.NoError(os.WriteFile(first, []byte(`{"thread_path":"inbox/circle","title":"Fictional circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[]}`), 0600))
		record := instagramTestRecord("old")
		record["sender_name"] = "Lumen Example"
		instagramTestShard(t, root, "circle", "message_2.json", record)
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		require.NoError(os.WriteFile(first, []byte(`{"thread_path":"inbox/circle","title":"Fictional circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"},{"name":"Lumen Example"}],"messages":[]}`), 0600))
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		var kind string
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
		require.Equal("group_chat", kind)
	})
}

func TestInstagramRejectsNewerFingerprintInAbsentThread(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	old := instagramTestShard(t, root, "old", "message_1.json", instagramTestRecord("original"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(os.Remove(old))
	instagramTestShard(t, root, "new", "message_1.json", instagramTestRecord("new"))
	for _, version := range []string{"ig:v2:", "ig:v10:"} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET source_message_id = ? || substr(source_message_id, 7)`), version)
		require.NoError(err)
		_, err = ImportDYI(t.Context(), st, opts)
		require.ErrorContains(err, "this archive uses an unsupported import format")
		assert.Equal(t, 1, countMessages(t, st, ""))
	}
}

func TestInstagramAddsOnlySurplusCopiesWithSharedReactions(t *testing.T) {
	for name, shared := range map[string]bool{"distinct": false, "shared": true} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
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
			require.NoError(err)
			original := instagramIDs(t, st)
			extra := []string{}
			if shared {
				extra = []string{"x"}
			}
			instagramTestShard(t, root, "circle", "message_1.json", copyOf(append([]string{"d"}, extra...)...), copyOf(append([]string{"e"}, extra...)...), copyOf(append([]string{"f"}, extra...)...))
			summary, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(int64(1), summary.MessagesAdded)
			assert.True(summary.Incomplete)
			assert.Equal(3, countMessages(t, st, ""))
			for _, id := range instagramIDs(t, st) {
				if !slices.Contains(original, id) {
					var messageID int64
					require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), id).Scan(&messageID))
					raw, err := st.GetMessageRawContext(t.Context(), messageID)
					require.NoError(err)
					for _, reaction := range []string{"d", "e", "f"} {
						assert.Contains(string(raw), `"reaction":"`+reaction+`"`)
					}
				}
			}
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('a','b')`).Scan(&count))
			assert.Equal(2, count)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('d','e','f')`).Scan(&count))
			assert.Zero(count)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value = 'x'`).Scan(&count))
			assert.Equal(len(extra), count)
			assert.Subset(instagramIDs(t, st), original)
			summary, err = ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(3, countMessages(t, st, ""))
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('d','e','f')`).Scan(&count))
			assert.Zero(count)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value = 'x'`).Scan(&count))
			assert.Equal(len(extra), count)
		})
	}
	t.Run("shared reaction is not identity", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		copyOf := func(reactions ...string) map[string]any {
			record := instagramTestRecord("same")
			var list []map[string]string
			for _, reaction := range reactions {
				list = append(list, map[string]string{"actor": reaction, "reaction": reaction})
			}
			record["reactions"] = list
			return record
		}
		instagramTestShard(t, root, "circle", "message_1.json", copyOf("A"))
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		id := instagramIDs(t, st)[0]
		instagramTestShard(t, root, "circle", "message_1.json", copyOf("B"), copyOf("B", "C"))
		for i := range 2 {
			summary, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.True(summary.Incomplete)
			if i == 0 {
				assert.Equal(int64(1), summary.MessagesAdded)
			} else {
				assert.Zero(summary.MessagesAdded)
			}
			assert.Equal(2, countMessages(t, st, ""))
			var reaction string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT r.reaction_value FROM reactions r JOIN messages m ON m.id = r.message_id WHERE m.source_message_id = ?`), id).Scan(&reaction))
			assert.Equal("A", reaction)
		}
		instagramTestShard(t, root, "circle", "message_1.json", copyOf("A"), copyOf("B", "C"))
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.True(summary.Incomplete)
		assert.Zero(summary.MessagesAdded)
		assert.Equal(2, countMessages(t, st, ""))
		var extra int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions WHERE reaction_value = 'C'`).Scan(&extra))
		assert.Zero(extra)
	})
	t.Run("distributed candidate evidence", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		copyOf := func(reaction string) map[string]any {
			record := instagramTestRecord("same")
			record["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": reaction}}
			return record
		}
		instagramTestShard(t, root, "circle", "message_1.json", copyOf("a"))
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		original := instagramIDs(t, st)
		instagramTestShard(t, root, "circle", "message_1.json", copyOf("d"), copyOf("e"), copyOf("f"))
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		check := func() {
			var candidates []string
			for _, id := range instagramIDs(t, st) {
				if slices.Contains(original, id) {
					continue
				}
				var messageID int64
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), id).Scan(&messageID))
				raw, err := st.GetMessageRawContext(t.Context(), messageID)
				require.NoError(err)
				var envelope instagramRaw
				require.NoError(json.Unmarshal(raw, &envelope))
				for _, candidate := range envelope.Candidates {
					var source instagramRaw
					require.NoError(json.Unmarshal(candidate, &source))
					var record rawJSONMessage
					require.NoError(json.Unmarshal(source.Record, &record))
					require.Len(record.Reactions, 1)
					candidates = append(candidates, record.Reactions[0].Reaction)
				}
			}
			assert.ElementsMatch([]string{"d", "e", "f"}, candidates)
		}
		check()
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		check()
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('d','e','f')`).Scan(&count))
		assert.Zero(count)
	})
}

func TestInstagramReportsConversationStatsFailure(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "SQLite trigger injects a stats update failure")
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("same"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_stats BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(FAIL, 'stats failed'); END`)
	require.NoError(err)
	_, err = ImportDYI(t.Context(), st, opts)
	require.ErrorContains(err, "stats failed")
}

func TestInstagramDistinctCallContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	call := instagramTestRecord("")
	call["type"] = "Call"
	call["call_duration"] = 60
	instagramTestShard(t, root, "circle", "message_1.json", call)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	call["call_duration"] = 120
	instagramTestShard(t, root, "circle", "message_1.json", call)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(2, countMessages(t, st, ""))
	assert.Equal(1, countMessages(t, st, "snippet='[call: 1m 0s]'"))
	assert.Equal(1, countMessages(t, st, "snippet='[call: 2m 0s]'"))
	call["missed"] = true
	instagramTestShard(t, root, "circle", "message_1.json", call)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(3, countMessages(t, st, ""))
	call["type"] = "Unsubscribe"
	instagramTestShard(t, root, "circle", "message_1.json", call)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(4, countMessages(t, st, ""))
}

func TestInstagramReactionRemoval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	record := instagramTestRecord("hello")
	record["reactions"] = []map[string]string{{"actor": "Reactiononly", "reaction": "✨"}}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var created sql.NullTime
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT created_at FROM reactions LIMIT 1`).Scan(&created))
	assert.False(created.Valid)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Reactiononly"}}, 0, 10)
	require.NoError(err)
	require.Equal(int64(1), total)
	delete(record, "reactions")
	instagramTestShard(t, root, "circle", "message_1.json", record)
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&n))
	assert.Zero(n)
	assert.Equal(1, countMessages(t, st, "snippet='hello'"))
	_, total, err = st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Reactiononly"}}, 0, 10)
	require.NoError(err)
	assert.Zero(total)
}

func TestInstagramExcludedCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("hello"))
	invalid := filepath.Join(root, filepath.FromSlash(instagramInbox), "invalid")
	require.NoError(os.MkdirAll(invalid, 0700))
	require.NoError(os.WriteFile(filepath.Join(invalid, "message_final.json"), []byte(`{}`), 0600))
	requests := filepath.Join(root, "your_instagram_activity", "messages", "message_requests", "request")
	require.NoError(os.MkdirAll(requests, 0700))
	require.NoError(os.WriteFile(filepath.Join(requests, "message_1.json"), []byte(`{"thread_path":"message_requests/request","participants":[{"name":"Mira Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"excluded request"}]}`), 0600))
	summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
	require.NoError(err)
	assert.True(summary.Incomplete)
	assert.Equal(int64(2), summary.FilesSkipped)
	assert.Equal(1, countMessages(t, st, ""))
	assert.Zero(countMessages(t, st, "snippet='excluded request'"))
}

func TestInstagramUnreadableThreadReportsIncomplete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "readable", "message_1.json", instagramTestRecord("hello"))
	filename := instagramTestShard(t, root, "unreadable", "message_1.json", instagramTestRecord("hidden"))
	dir := filepath.Dir(filename)
	require.NoError(os.Chmod(dir, 0))
	t.Cleanup(func() { require.NoError(os.Chmod(dir, 0700)) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("process can read a directory without permissions")
	}
	summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
	require.NoError(err)
	assert.True(summary.Incomplete)
	assert.Equal(int64(1), summary.FilesSkipped)
	assert.Equal(1, countMessages(t, st, ""))
}

func TestInstagramSenderChangesRetainMessages(t *testing.T) {
	for _, tc := range []struct {
		name, sender  string
		rename, media bool
	}{
		{name: "rename", sender: "Renamed Example", rename: true},
		{name: "different sender", sender: "Lumen Example"},
		{name: "different media", sender: "Mira Example", media: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			record := instagramTestRecord("hello")
			if tc.media {
				record["photos"] = []map[string]string{{"uri": "photos/one.jpg"}}
			}
			filename := instagramTestShard(t, root, "circle", "message_1.json", record)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			ids := instagramIDs(t, st)
			record["sender_name"] = tc.sender
			if tc.media {
				record["photos"] = []map[string]string{{"uri": "photos/two.jpg"}}
			}
			data, err := os.ReadFile(filename)
			require.NoError(err)
			var shard map[string]any
			require.NoError(json.Unmarshal(data, &shard))
			shard["messages"] = []map[string]any{record}
			if tc.rename {
				shard["participants"] = []map[string]string{{"name": "Nova Example"}, {"name": tc.sender}}
			}
			data, err = json.Marshal(shard)
			require.NoError(err)
			require.NoError(os.WriteFile(filename, data, 0600))
			summary, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(int64(1), summary.MessagesAdded)
			assert.Equal(2, countMessages(t, st, ""))
			assert.Subset(instagramIDs(t, st), ids)
			ids = instagramIDs(t, st)
			summary, err = ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(ids, instagramIDs(t, st))
		})
	}
}

func TestInstagramDuplicateOccurrences(t *testing.T) {
	t.Run("reordered", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		a, b := instagramTestRecord("hello"), instagramTestRecord("hello")
		a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "✨"}}
		b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "❤️"}}
		instagramTestShard(t, root, "circle", "message_1.json", a, b)
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		reactions := func() map[string]string {
			rows, err := st.DB().QueryContext(t.Context(), `SELECT m.source_message_id, r.reaction_value FROM messages m JOIN reactions r ON r.message_id = m.id`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			result := map[string]string{}
			for rows.Next() {
				var id, reaction string
				require.NoError(rows.Scan(&id, &reaction))
				assert.NotContains(result, id)
				result[id] = reaction
			}
			require.NoError(rows.Err())
			return result
		}
		before := reactions()
		instagramTestShard(t, root, "circle", "message_1.json", b)
		instagramTestShard(t, root, "circle", "message_2.json", a)
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&count))
		assert.Equal(2, count)
		assert.Equal(before, reactions())
		require.NoError(os.Remove(filepath.Join(root, filepath.FromSlash(instagramInbox), "circle", "message_2.json")))
		original := instagramIDs(t, st)
		instagramTestShard(t, root, "circle", "message_1.json", b)
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
		assert.Equal(original, instagramIDs(t, st))
		c := instagramTestRecord("hello")
		c["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "c"}}
		instagramTestShard(t, root, "circle", "message_1.json", a, c)
		summary, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
		assert.Equal(original, instagramIDs(t, st))
	})

	t.Run("ambiguous direction repair", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		a, b := instagramTestRecord("same"), instagramTestRecord("same")
		a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "a"}}
		b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "b"}}
		instagramTestShard(t, root, "circle", "message_1.json", a, b)
		opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "wrong"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		ids := instagramIDs(t, st)
		snapshot := func() (map[string][]byte, map[string]string) {
			rows, err := st.DB().QueryContext(t.Context(), `SELECT m.id, m.source_message_id, r.reaction_value FROM messages m JOIN reactions r ON r.message_id = m.id`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			raws, reactions := map[string][]byte{}, map[string]string{}
			for rows.Next() {
				var id int64
				var sourceID, reaction string
				require.NoError(rows.Scan(&id, &sourceID, &reaction))
				raws[sourceID], err = st.GetMessageRawContext(t.Context(), id)
				require.NoError(err)
				reactions[sourceID] = reaction
			}
			require.NoError(rows.Err())
			return raws, reactions
		}
		raws, reactions := snapshot()
		a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "c"}}
		b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "d"}}
		instagramTestShard(t, root, "circle", "message_1.json", a, b)
		opts.SelfName = "Mira Example"
		for range 2 {
			summary, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.True(summary.Incomplete)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(ids, instagramIDs(t, st))
			assert.Equal(2, countMessages(t, st, "is_from_me = TRUE"))
			run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
			require.NoError(err)
			assert.Positive(run.ErrorsCount)
			afterRaw, afterReactions := snapshot()
			assert.Equal(reactions, afterReactions)
			assert.Equal(raws, afterRaw)
		}
	})

	t.Run("unreadable evidence", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("same"))
		opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		var raw []byte
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT raw_data FROM message_raw`).Scan(&raw))
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_raw SET raw_data = ?`), []byte("not-zlib"))
		require.NoError(err)
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
		assert.True(summary.Incomplete)
		assert.Equal(1, countMessages(t, st, ""))
		run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
		require.NoError(err)
		assert.Positive(run.ErrorsCount)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_raw SET raw_data = ?`), raw)
		require.NoError(err)
		summary, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
		assert.False(summary.Incomplete)
		run, err = st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
		require.NoError(err)
		assert.Zero(run.ErrorsCount)
	})
}
