package fbmessenger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInstagramFingerprint(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("39151bedba35d941336e56ea21fd767503efe5c6f0ddc63dcfd58ffaaa600733", instagramFingerprint(rawJSONMessage{SenderName: "Nova <Example>", Content: "&\u2028\u2029"}))
	assert.Equal("11a22c256e70769d0395b256aab31a3ef6a1c94d23406ee0dd114fa485ea9aa8", instagramFingerprint(rawJSONMessage{
		SenderName: "Nova Example", TimestampMs: 1730500000123, Content: "hello",
		Share:  &rawJSONShare{Link: "https://example.org/x", ShareText: "A & B", OriginalContentOwner: "Mira Example"},
		Photos: []rawJSONPhoto{{URI: "photos/a.jpg"}}, Media: []rawJSONPhoto{{URI: "docs/one.pdf"}},
	}))
	assert.Equal("0d95a897c44227897c749a3d6e24e5ad2bee36f48e8b12e70237a60d18955485", instagramFingerprint(rawJSONMessage{
		Audio: []rawJSONPhoto{{URI: "a.ogg"}}, Videos: []rawJSONPhoto{{URI: "b.mp4"}},
		Gifs: []rawJSONPhoto{{URI: "c.gif"}}, Sticker: &rawJSONSticker{URI: "d.webp"},
	}))
}

func TestInstagramImportReplay(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Source: "instagram", RootDir: "testdata/instagram", Account: "orbit.example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()}
	summary, err := ImportDYI(t.Context(), st, opts)
	must.NoError(err)
	checks.Equal(int64(6), summary.MessagesAdded)
	checks.Equal(2, summary.ThreadsProcessed)
	checks.Equal(1, summary.MediaMissing)
	checks.Equal(1, countMessages(t, st, "is_from_me = TRUE"))
	beforeRevision, err := st.DerivedDataRevisionContext(t.Context())
	must.NoError(err)
	summary, err = ImportDYI(t.Context(), st, opts)
	must.NoError(err)
	checks.Zero(summary.MessagesAdded)
	afterRevision, err := st.DerivedDataRevisionContext(t.Context())
	must.NoError(err)
	checks.Equal(beforeRevision, afterRevision)
	checks.Equal(6, countMessages(t, st, "message_type='instagram'"))
	var timestamp time.Time
	must.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sent_at FROM messages WHERE snippet='The telescope is ready.'`).Scan(&timestamp))
	checks.Equal(int64(1730500000123), timestamp.UnixMilli())
	var id int64
	must.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE snippet LIKE 'café%'`).Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	must.NoError(err)
	checks.Contains(string(raw), "message_1.json")
	checks.Contains(string(raw), "timestamp_ms")
	checks.Contains(string(raw), `caf\u00c3\u00a9`)
	ids := instagramIDs(t, st)
	checks.Contains(ids, "ig:v1:9eb6fb43c374a0f75d8413e28359697c136b8c31492b361d95a71f2d60998e6e:f381d2c678356978d8dd328d259fb5c61f79f27b7cfd885a661ba280c8416ef0:0")
	checks.Contains(ids, "ig:v1:9eb6fb43c374a0f75d8413e28359697c136b8c31492b361d95a71f2d60998e6e:3225a563445b0a8cb0a916f3543b430e6a707b78759cc7c507f39c50846472cb:0")
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
	raws, _ := instagramSnapshot(t, st)
	return slices.Sorted(maps.Keys(raws))
}

// instagramSnapshot returns each message's raw record and reactions by source ID.
func instagramSnapshot(t *testing.T, st *store.Store) (raws, reactions map[string]string) {
	t.Helper()
	require := require.New(t)
	rows, err := st.DB().QueryContext(t.Context(), `SELECT m.id, m.source_message_id, COALESCE(r.reaction_value, '') FROM messages m LEFT JOIN reactions r ON r.message_id = m.id ORDER BY r.reaction_value`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	raws, reactions = map[string]string{}, map[string]string{}
	for rows.Next() {
		var id int64
		var sourceID, reaction string
		require.NoError(rows.Scan(&id, &sourceID, &reaction))
		raw, err := st.GetMessageRawContext(t.Context(), id)
		require.NoError(err)
		raws[sourceID], reactions[sourceID] = string(raw), reactions[sourceID]+reaction
	}
	require.NoError(rows.Err())
	return raws, reactions
}

// instagramCopy is one copy of the same record with each reaction by its own actor.
func instagramCopy(reactions ...string) map[string]any {
	record := instagramTestRecord("same")
	var list []map[string]string
	for _, reaction := range reactions {
		list = append(list, map[string]string{"actor": reaction, "reaction": reaction})
	}
	record["reactions"] = list
	return record
}

func TestInstagramAccountAndExactNameIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "dm", "message_1.json", instagramTestRecord("same person in two chats"))
	group := instagramTestShard(t, root, "group", "message_1.json")
	require.NoError(os.WriteFile(group, []byte(`{"thread_path":"inbox/group","title":"Fictional circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"},{"name":"Mira.Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"same person in two chats"}]}`), 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "@EXAMPLE", SelfName: "mira example"}
	_, err := ImportDYI(t.Context(), st, opts)
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

func TestInstagramSenderMissingFromHeader(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json")
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","title":"Fictional circle","participants":[{"name":"Nova Example"},{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"},{"sender_name":"Mira Example","timestamp_ms":1730500000124,"content":"incoming"}]}`), 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	for range 2 {
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		var recipients int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE m.snippet = 'outgoing' AND r.recipient_type = 'to' AND r.display_name = 'Mira Example'`).Scan(&recipients))
		assert.Equal(1, recipients)
	}
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","title":"Fictional circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"}]}`), 0600))
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var recipients int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE m.snippet = 'outgoing' AND r.recipient_type = 'to' AND r.display_name = 'Mira Example'`).Scan(&recipients))
	assert.Equal(1, recipients)
}

func TestInstagramConversationTypeIncludesRetainedSenders(t *testing.T) {
	for _, tc := range []struct{ name, header string }{
		{"sender missing from header", `[{"name":"Nova Example"},{"name":"Mira Example"}]`},
		{"full header", `[{"name":"Nova Example"},{"name":"Mira Example"},{"name":"Lumen Example"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, root := testutil.NewTestStore(t), t.TempDir()
			filename := instagramTestShard(t, root, "circle", "message_1.json")
			initial := `{"thread_path":"inbox/circle","participants":` + tc.header + `,"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"},{"sender_name":"Lumen Example","timestamp_ms":1730500000124,"content":"incoming"}]}`
			require.NoError(os.WriteFile(filename, []byte(initial), 0600))
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			var kind string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
			assert.Equal("group_chat", kind)
			partial := `{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"}]}`
			require.NoError(os.WriteFile(filename, []byte(partial), 0600))
			_, err = ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
			assert.Equal("group_chat", kind)
		})
	}
}

func TestInstagramReplayDiscoversSenderMissingFromHeader(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json")
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"}]}`), 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"},{"sender_name":"Mira Example","timestamp_ms":1730500000124,"content":"incoming"}]}`), 0600))
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var recipients int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE m.snippet = 'outgoing' AND r.recipient_type = 'to' AND r.display_name = 'Mira Example'`).Scan(&recipients))
	assert.Equal(1, recipients)
}

func TestInstagramReplayKeepsEditedParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json")
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"},{"sender_name":"Mira Example","timestamp_ms":1730500000124,"content":"incoming"}]}`), 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE participants SET display_name = 'Mira Renamed' WHERE display_name = 'Mira Example'`)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"}]}`), 0600))
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var participants int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participants`).Scan(&participants))
	assert.Equal(2, participants)
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"},{"sender_name":"Mira Example","timestamp_ms":1730500000124,"content":"incoming"}]}`), 0600))
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var selfRecipients int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE m.snippet = 'incoming' AND r.recipient_type = 'to' AND r.participant_id = m.sender_id`).Scan(&selfRecipients))
	assert.Zero(selfRecipients)
}

func TestInstagramReplayParticipantNameCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json")
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"},{"sender_name":"Mira Example","timestamp_ms":1730500000124,"content":"incoming"}]}`), 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var sender, recipient int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE snippet = 'outgoing'`).Scan(&sender))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE snippet = 'incoming'`).Scan(&recipient))
	_, err = st.DB().ExecContext(t.Context(), `UPDATE participants SET display_name = 'Nova Example' WHERE display_name = 'Mira Example'`)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"}],"messages":[{"sender_name":"Nova Example","timestamp_ms":1730500000123,"content":"outgoing"}]}`), 0600))
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var after int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE snippet = 'outgoing'`).Scan(&after))
	assert.Equal(sender, after)
	var recipients int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id WHERE m.snippet = 'outgoing' AND r.recipient_type = 'to' AND r.participant_id = ?`), recipient).Scan(&recipients))
	assert.Equal(1, recipients)
}

func TestInstagramLimitReplay(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("one"), instagramTestRecord("two"), instagramTestRecord("three"), instagramTestRecord("four"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example", Limit: 1}
	assert := assert.New(t)
	require := require.New(t)
	for i := 1; i <= 3; i++ {
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.True(summary.LimitReached)
		run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
		require.NoError(err)
		assert.Equal(summary.MessagesProcessed, run.MessagesProcessed)
		assert.Equal(store.SyncStatusPaused, run.Status)
		assert.Equal(int64(1), run.MessagesAdded)
		assert.Empty(run.CursorBefore.String)

		assert.Equal(int64(1), summary.MessagesAdded)
		assert.Equal(i, countMessages(t, st, ""))
		if i == 1 {
			_, err = st.RebuildFTS(nil)
			require.NoError(err)
			_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Fictional"}}, 0, 10)
			require.NoError(err)
			assert.Equal(int64(1), total)
		}
	}
}

func TestInstagramMissingSenderAndTimestamp(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "circle", "message_1.json", map[string]any{"content": "no timestamp"}, map[string]any{"timestamp_ms": 0, "content": "epoch unknown sender"}, map[string]any{"timestamp_ms": "yesterday", "content": "invalid time"})
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	assert := assert.New(t)
	require := require.New(t)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
	require.NoError(err)
	assert.Equal(int64(2), run.ErrorsCount)
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
	assert := assert.New(t)
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
	assert.True(summary.Incomplete)
	assert.Equal(7, summary.MediaRejected)
	assert.Equal(1, countMessages(t, st, ""))
}

func TestInstagramDuplicateMediaSlot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("photo"), 0600))
	record := instagramTestRecord("duplicate photo")
	record["photos"] = []map[string]string{{"uri": "photo.jpg"}, {"uri": "photo.jpg"}}
	instagramTestShard(t, root, "circle", "message_1.json", record)
	summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()})
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&count))
	assert.Equal(1, count)
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

func TestInstagramRejectsBeforeSource(t *testing.T) {
	for _, test := range []struct {
		name, account, selfName, format, wantError string
		limit                                      int
		unsupported                                bool
	}{
		{name: "empty account", selfName: "Nova Example", wantError: "account"},
		{name: "blank normalized account", account: "@", selfName: "Nova Example", wantError: "account"},
		{name: "repeated account prefix", account: "@@example", selfName: "Nova Example", wantError: "account"},
		{name: "blank self name", account: "example", selfName: "   ", wantError: "self-name"},
		{name: "negative limit", account: "example", selfName: "Nova Example", limit: -1, wantError: "--limit must be non-negative"},
		{name: "unsupported format", account: "example", selfName: "Nova Example", format: "html", wantError: "valid: auto, json"},
		{name: "unsupported layout", account: "example", selfName: "Nova Example", wantError: "no supported inbox message_<N>.json files", unsupported: true},
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
			_, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: test.account, SelfName: test.selfName, Format: test.format, Limit: test.limit})
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
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Fictional"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(1, summary.ReactionsAdded)
}

func TestInstagramCanceledReplayWithoutTitleKeepsTitleSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("hello"))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&id))
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"hello"}]}`), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		raw, err := st.GetMessageRawContext(t.Context(), id)
		require.NoError(err)
		return strings.Contains(string(raw), `"title":""`)
	})
	_, err = ImportDYI(ctx, st, opts)
	require.ErrorIs(err, context.Canceled)
	require.True(canceled())
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Fictional"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
}

func TestInstagramRetryAfterCanceledRenameRepairsTitle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("first"), instagramTestRecord("second"))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","title":"Updated circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"third"}]}`), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE subject = 'Updated circle'`).Scan(&n))
		return n == 2
	})
	_, err = ImportDYI(ctx, st, opts)
	require.ErrorIs(err, context.Canceled)
	require.True(canceled())
	var title string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title FROM conversations`).Scan(&title))
	assert.Equal("Updated circle", title)
	var mixed int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE COALESCE(subject, '') <> ?`), title).Scan(&mixed))
	assert.Zero(mixed)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Updated"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(2), total)
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, total, err = st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Updated"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(2), total)
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title FROM conversations`).Scan(&title))
	assert.Equal("Updated circle", title)
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, total, err = st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Updated"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(3), total)
}

func TestInstagramRenameRollsBackWhenSubjectWriteFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "SQLite trigger injects a subject write failure")
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("first"), instagramTestRecord("second"))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_subject BEFORE UPDATE OF subject ON messages WHEN NEW.subject = 'Updated circle' AND NEW.id = (SELECT MAX(id) FROM messages) BEGIN SELECT RAISE(FAIL, 'subject failed'); END`)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","title":"Updated circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"third"}]}`), 0600))
	_, err = ImportDYI(t.Context(), st, opts)
	require.ErrorContains(err, "subject failed")
	var title string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title FROM conversations`).Scan(&title))
	assert.Equal("Fictional circle", title)
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Fictional"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(2), total)
}

func TestInstagramSharedLink(t *testing.T) {
	for _, kind := range []string{"", "Share"} {
		t.Run("type="+kind, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			record := instagramTestRecord("")
			record["type"] = kind
			record["share"] = map[string]string{"link": "https://example.com/cafÃ©"}
			instagramTestShard(t, root, "circle", "message_1.json", record)
			_, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
			require.NoError(t, err)
			var body string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
			assert.Equal(t, "[shared link] https://example.com/café", body)
		})
	}
}

func TestInstagramReplayChangedUnknownMetadata(t *testing.T) {
	for _, tc := range []struct {
		name              string
		distinct, partial bool
	}{
		{name: "distinct messages", distinct: true},
		{name: "identical copies"},
		{name: "partial identical copies", partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, root := testutil.NewTestStore(t), t.TempDir()
			a, b := instagramTestRecord("same"), instagramTestRecord("same")
			if tc.distinct {
				a["content"], b["content"] = "first", "second"
			}
			instagramTestShard(t, root, "circle", "message_1.json", a, b)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			ids := instagramIDs(t, st)
			require.Len(ids, 2)
			for _, value := range []bool{false, true, true} {
				before, _ := instagramSnapshot(t, st)
				a["is_geoblocked_for_viewer"], b["is_geoblocked_for_viewer"] = value, value
				records := []map[string]any{b, a}
				if tc.partial {
					records = records[:1]
				}
				instagramTestShard(t, root, "circle", "message_1.json", records...)
				summary, err := ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				assert.Zero(summary.MessagesAdded)
				assert.Equal(tc.partial && value, summary.Incomplete)
				assert.Equal(ids, instagramIDs(t, st))
				if tc.partial && value {
					after, _ := instagramSnapshot(t, st)
					assert.Equal(before, after)
				}
				if tc.distinct {
					raws, _ := instagramSnapshot(t, st)
					for _, raw := range raws {
						if value {
							assert.Contains(raw, `"is_geoblocked_for_viewer":true`)
						} else {
							assert.Contains(raw, `"is_geoblocked_for_viewer":false`)
						}
					}
				}
			}
		})
	}
}

func TestInstagramReplayChangedMediaEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, kind, field, want string
		before, after           any
	}{
		{"photo timestamp", "photos", "creation_timestamp", `"creation_timestamp":200`, 100, 200},
		{"video timestamp", "videos", "creation_timestamp", `"creation_timestamp":200`, 100, 200},
		{"photo metadata", "photos", "unknown", `"unknown":"changed"`, nil, "changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, root := testutil.NewTestStore(t), t.TempDir()
			record := instagramTestRecord("same")
			media := map[string]any{"uri": "media.jpg", "creation_timestamp": 100}
			if tc.before != nil {
				media[tc.field] = tc.before
			}
			record[tc.kind] = []map[string]any{media}
			instagramTestShard(t, root, "circle", "message_1.json", record)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			ids := instagramIDs(t, st)
			require.Len(ids, 1)
			media[tc.field] = tc.after
			instagramTestShard(t, root, "circle", "message_1.json", record)
			for range 2 {
				summary, err := ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				assert.Zero(summary.MessagesAdded)
				assert.Equal(ids, instagramIDs(t, st))
				raws, _ := instagramSnapshot(t, st)
				assert.Contains(raws[ids[0]], tc.want)
			}
		})
	}
}

func TestInstagramPartialReplayPreservesDistinctEvidence(t *testing.T) {
	for _, kind := range []string{"media timestamps", "message metadata", "media metadata and reactions"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, root := testutil.NewTestStore(t), t.TempDir()
			a, b := instagramTestRecord("same"), instagramTestRecord("same")
			switch kind {
			case "media timestamps":
				a["photos"] = []map[string]any{{"uri": "photo.jpg", "creation_timestamp": 100}}
				b["photos"] = []map[string]any{{"uri": "photo.jpg", "creation_timestamp": 200}}
			case "message metadata":
				a["extra_field"], b["extra_field"] = "first", "second"
			case "media metadata and reactions":
				a["photos"] = []map[string]any{{"uri": "photo.jpg", "creation_timestamp": 100, "extra": "first"}}
				b["photos"] = []map[string]any{{"uri": "photo.jpg", "creation_timestamp": 100, "extra": "second"}}
				a["reactions"], b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "a"}}, []map[string]string{{"actor": "Nova Example", "reaction": "b"}}
			}
			instagramTestShard(t, root, "circle", "message_1.json", a, b)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			before, reactions := instagramSnapshot(t, st)
			require.Len(before, 2)
			if kind == "media metadata and reactions" {
				instagramTestShard(t, root, "circle", "message_1.json", b, a)
				_, err = ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				after, _ := instagramSnapshot(t, st)
				assert.Equal(before, after)
			}
			switch kind {
			case "media timestamps":
				a["photos"] = []map[string]any{{"uri": "photo.jpg", "creation_timestamp": 300}}
			case "message metadata":
				a["extra_field"] = "third"
			case "media metadata and reactions":
				a["photos"] = []map[string]any{{"uri": "photo.jpg", "creation_timestamp": 100, "extra": "changed"}}
			}
			instagramTestShard(t, root, "circle", "message_1.json", a)
			for range 2 {
				summary, err := ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				assert.True(summary.Incomplete)
				assert.Zero(summary.MessagesAdded)
				after, afterReactions := instagramSnapshot(t, st)
				assert.Equal(before, after)
				assert.Equal(reactions, afterReactions)
			}
		})
	}
}

func TestInstagramEqualCopiesWithChangedMetadata(t *testing.T) {
	for _, distinct := range []bool{false, true} {
		t.Run(fmt.Sprintf("distinct old metadata %t", distinct), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, root := testutil.NewTestStore(t), t.TempDir()
			a, b := instagramTestRecord("same"), instagramTestRecord("same")
			if distinct {
				a["extra_field"], b["extra_field"] = "old a", "old b"
			}
			instagramTestShard(t, root, "circle", "message_1.json", a, b)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			before, _ := instagramSnapshot(t, st)
			a["extra_field"], b["extra_field"] = "new a", "new b"
			instagramTestShard(t, root, "circle", "message_1.json", b, a)
			for range 2 {
				summary, err := ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				assert.True(summary.Incomplete)
				assert.Zero(summary.MessagesAdded)
				after, _ := instagramSnapshot(t, st)
				assert.Equal(before, after)
			}
		})
	}
}

func TestInstagramReplayAddsSurplusWithChangedMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	original := instagramCopy("old")
	a, b := instagramCopy("first", "shared"), instagramCopy("second", "shared")
	instagramTestShard(t, root, "circle", "message_1.json", original)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	before, _ := instagramSnapshot(t, st)
	a["is_geoblocked_for_viewer"], b["is_geoblocked_for_viewer"] = false, false
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.True(summary.Incomplete)
	require.Equal(int64(1), summary.MessagesAdded)
	after, _ := instagramSnapshot(t, st)
	require.Len(after, 2)
	for id, raw := range before {
		assert.Equal(raw, after[id])
	}
	var candidateReactions []string
	for id, raw := range after {
		if _, exists := before[id]; exists {
			continue
		}
		var envelope instagramRaw
		require.NoError(json.Unmarshal([]byte(raw), &envelope))
		for _, candidate := range envelope.Candidates {
			var source instagramRaw
			var record rawJSONMessage
			require.NoError(json.Unmarshal(candidate, &source))
			require.NoError(json.Unmarshal(source.Record, &record))
			require.Len(record.Reactions, 2)
			candidateReactions = append(candidateReactions, record.Reactions[0].Reaction)
		}
	}
	assert.ElementsMatch([]string{"first", "second"}, candidateReactions)
	var retained, shared, unshared int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions WHERE reaction_value = 'old'`).Scan(&retained))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions WHERE reaction_value = 'shared'`).Scan(&shared))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions WHERE reaction_value IN ('first', 'second')`).Scan(&unshared))
	assert.Equal(1, retained)
	assert.Equal(1, shared)
	assert.Zero(unshared)
	instagramTestShard(t, root, "circle", "message_1.json", b, a)
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.True(summary.Incomplete)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(2, countMessages(t, st, ""))
}

func TestInstagramReplaySurplusPreservesDistinctMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	a, b, c := instagramTestRecord("same"), instagramTestRecord("same"), instagramTestRecord("same")
	a["extra_field"], b["extra_field"] = "first", "second"
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	before, _ := instagramSnapshot(t, st)
	a["extra_field"], b["extra_field"], c["extra_field"] = "third", "third", "third"
	instagramTestShard(t, root, "circle", "message_1.json", c, b, a)
	for i := range 2 {
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.True(summary.Incomplete)
		assert.Equal(int64(1-i), summary.MessagesAdded)
		after, _ := instagramSnapshot(t, st)
		assert.Len(after, 3)
		for id, raw := range before {
			assert.Equal(raw, after[id])
		}
	}
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(3, countMessages(t, st, ""))
}

func TestInstagramReplayChangedMetadataPreservesDistinctReactionEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	a, b := instagramTestRecord("same"), instagramTestRecord("same")
	a["reactions"] = []map[string]any{{"actor": "Nova Example", "reaction": "a", "extra_field": "first"}}
	b["reactions"] = []map[string]any{{"actor": "Nova Example", "reaction": "a", "extra_field": "second"}}
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	before, _ := instagramSnapshot(t, st)
	a["is_geoblocked_for_viewer"], b["is_geoblocked_for_viewer"] = false, false
	a["reactions"], b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "a"}}, []map[string]string{{"actor": "Nova Example", "reaction": "a"}}
	instagramTestShard(t, root, "circle", "message_1.json", b, a)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	after, _ := instagramSnapshot(t, st)
	assert.Len(after, 2)
	for id, raw := range before {
		assert.Equal(raw, after[id])
	}
}

func TestInstagramLaterTitleIndexesMessagesWithoutSubjects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	filename := instagramTestShard(t, root, "circle", "message_1.json")
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"first"},{"sender_name":"Mira Example","timestamp_ms":1730500000124,"content":"second"}]}`), 0600))
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET embed_gen = 7`)
	require.NoError(err)
	require.NoError(os.WriteFile(filename, []byte(`{"thread_path":"inbox/circle","title":"Updated circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"first"}]}`), 0600))
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	var stale int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE embed_gen IS NULL`).Scan(&stale))
	assert.Equal(2, stale)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET embed_gen = 7`)
	require.NoError(err)
	_, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE embed_gen = 7`).Scan(&stale))
	assert.Equal(2, stale)
	_, err = st.RebuildFTS(nil)
	require.NoError(err)
	_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Updated"}}, 0, 10)
	require.NoError(err)
	assert.Equal(int64(2), total)
}

func TestInstagramConversationUsesFirstShardHeader(t *testing.T) {
	t.Run("header and raw provenance", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		first := instagramTestShard(t, root, "circle", "message_1.json")
		require.NoError(os.WriteFile(first, []byte(`{"thread_path":"inbox/circle","title":"Initial title","participants":[{"name":"Nova Example"},{"name":"Mira Example"},{"name":"Lumen Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"new"}]}`), 0600))
		second := instagramTestShard(t, root, "circle", "message_2.json")
		require.NoError(os.WriteFile(second, []byte(`{"thread_path":"inbox/circle","title":"Old title","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"old"}]}`), 0600))
		opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
		for range 2 {
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			var title, kind string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title, conversation_type FROM conversations`).Scan(&title, &kind))
			assert.Equal("Initial title", title)
			assert.Equal("group_chat", kind)
			_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Initial"}}, 0, 10)
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
		assert.Equal("group_chat", kind)
		_, total, err := st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Latest"}}, 0, 10)
		require.NoError(err)
		assert.Equal(int64(2), total)
		_, total, err = st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Initial"}}, 0, 10)
		require.NoError(err)
		assert.Zero(total)
		require.NoError(os.WriteFile(first, []byte(`{"thread_path":"inbox/circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"}],"messages":[]}`), 0600))
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		_, total, err = st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Latest"}}, 0, 10)
		require.NoError(err)
		assert.Equal(int64(2), total)
		_, err = st.RebuildFTS(nil)
		require.NoError(err)
		_, total, err = st.SearchMessagesQueryContext(t.Context(), &search.Query{TextTerms: []string{"Latest"}}, 0, 10)
		require.NoError(err)
		assert.Equal(int64(2), total)
	})
	t.Run("empty first shard type change", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		first := instagramTestShard(t, root, "circle", "message_1.json")
		require.NoError(os.WriteFile(first, []byte(`{"thread_path":"inbox/circle","title":"Fictional circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"},{"name":""}],"messages":[]}`), 0600))
		record := instagramTestRecord("old")
		record["sender_name"] = "Mira Example"
		instagramTestShard(t, root, "circle", "message_2.json", record)
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		var kind string
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
		require.Equal("direct_chat", kind)
		require.NoError(os.WriteFile(first, []byte(`{"thread_path":"inbox/circle","title":"Fictional circle","participants":[{"name":"Nova Example"},{"name":"Mira Example"},{"name":"Lumen Example"}],"messages":[]}`), 0600))
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
		require.Equal("group_chat", kind)
	})
}

func TestInstagramRejectsNewerFingerprintInAbsentThread(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	old := instagramTestShard(t, root, "old", "message_1.json", instagramTestRecord("original"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	summary, err := ImportDYI(t.Context(), st, opts)
	must.NoError(err)
	must.NoError(os.Remove(old))
	instagramTestShard(t, root, "new", "message_1.json", instagramTestRecord("new"))
	for _, version := range []string{"ig:v2:", "ig:v10:"} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET source_message_id = ? || substr(source_message_id, 7)`), version)
		must.NoError(err)
		_, err = ImportDYI(t.Context(), st, opts)
		must.ErrorContains(err, "this archive uses an unsupported import format; upgrade msgvault")
		run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
		must.NoError(err)
		checks.Equal(store.SyncStatusFailed, run.Status)
		checks.Equal(1, countMessages(t, st, ""))
	}
}

func TestInstagramRejectsConcurrentImport(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("original"))
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(t, err)
	syncID, err := st.StartSyncContext(t.Context(), summary.SourceID, "import-instagram")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, st.FailSyncContext(context.WithoutCancel(t.Context()), syncID, "test cleanup"))
	})
	_, err = ImportDYI(t.Context(), st, opts)
	assert.ErrorIs(t, err, store.ErrSyncAlreadyActive)
}

func TestInstagramAddsOnlySurplusCopiesWithSharedReactions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		old   int
	}{{"one distinct", nil, 1}, {"one shared", []string{"x"}, 1}, {"two distinct", nil, 2}, {"two shared", []string{"x"}, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			extra := tc.extra
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			originals := []map[string]any{instagramCopy("a")}
			if tc.old == 2 {
				originals = append(originals, instagramCopy("b"))
			}
			instagramTestShard(t, root, "circle", "message_1.json", originals...)
			opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			original := instagramIDs(t, st)
			instagramTestShard(t, root, "circle", "message_1.json", instagramCopy(append([]string{"d"}, extra...)...), instagramCopy(append([]string{"e"}, extra...)...), instagramCopy(append([]string{"f"}, extra...)...))
			for i := range 2 {
				summary, err := ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				assert.Equal(int64((3-tc.old)*(1-i)), summary.MessagesAdded)
				assert.True(summary.Incomplete)
				assert.Equal(3, countMessages(t, st, ""))
				assert.Subset(instagramIDs(t, st), original)
				var candidates []string
				raws, _ := instagramSnapshot(t, st)
				for id, raw := range raws {
					if slices.Contains(original, id) {
						continue
					}
					var envelope instagramRaw
					require.NoError(json.Unmarshal([]byte(raw), &envelope))
					for _, candidate := range envelope.Candidates {
						var source instagramRaw
						require.NoError(json.Unmarshal(candidate, &source))
						var record rawJSONMessage
						require.NoError(json.Unmarshal(source.Record, &record))
						require.Len(record.Reactions, 1+len(extra))
						candidates = append(candidates, record.Reactions[0].Reaction)
					}
				}
				assert.ElementsMatch([]string{"d", "e", "f"}, candidates)
				var count int
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('a','b')`).Scan(&count))
				assert.Equal(tc.old, count)
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value IN ('d','e','f')`).Scan(&count))
				assert.Zero(count)
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions WHERE reaction_value = 'x'`).Scan(&count))
				assert.Equal((3-tc.old)*len(extra), count)
			}
		})
	}
	t.Run("shared reaction is not identity", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		instagramTestShard(t, root, "circle", "message_1.json", instagramCopy("A"))
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		id := instagramIDs(t, st)[0]
		instagramTestShard(t, root, "circle", "message_1.json", instagramCopy("B"), instagramCopy("B", "C"))
		for i := range 2 {
			summary, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			assert.True(summary.Incomplete)
			assert.Equal(int64(1-i), summary.MessagesAdded)
			assert.Equal(2, countMessages(t, st, ""))
			var reaction string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT r.reaction_value FROM reactions r JOIN messages m ON m.id = r.message_id WHERE m.source_message_id = ?`), id).Scan(&reaction))
			assert.Equal("A", reaction)
		}
		instagramTestShard(t, root, "circle", "message_1.json", instagramCopy("A"), instagramCopy("B", "C"))
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.True(summary.Incomplete)
		assert.Zero(summary.MessagesAdded)
		assert.Equal(2, countMessages(t, st, ""))
		var extra int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reactions WHERE reaction_value = 'C'`).Scan(&extra))
		assert.Zero(extra)
	})
}

func TestInstagramSurplusBecomesIdentifiable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
	instagramTestShard(t, root, "circle", "message_1.json", instagramCopy("old"))
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	instagramTestShard(t, root, "circle", "message_1.json", instagramCopy("first"), instagramCopy("second"))
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.True(summary.Incomplete)
	before, _ := instagramSnapshot(t, st)
	require.Len(before, 2)
	var surplusID string
	var candidates []json.RawMessage
	for id, raw := range before {
		var envelope instagramRaw
		require.NoError(json.Unmarshal([]byte(raw), &envelope))
		if envelope.DerivedReactions != nil {
			surplusID = id
			candidates = append(candidates, envelope.Candidates...)
		}
	}
	require.NotEmpty(surplusID)
	require.Len(candidates, 2)
	instagramTestShard(t, root, "circle", "message_1.json", instagramCopy("old"), instagramCopy("unmatched"))
	summary, err = ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.True(summary.Incomplete)
	assert.Zero(summary.MessagesAdded)
	unchanged, _ := instagramSnapshot(t, st)
	assert.Equal(before, unchanged)
	instagramTestShard(t, root, "circle", "message_1.json", instagramCopy("old"), instagramCopy("second"))
	for range 2 {
		summary, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.False(summary.Incomplete)
		assert.Zero(summary.MessagesAdded)
		raws, reactions := instagramSnapshot(t, st)
		assert.Len(raws, 2)
		assert.Equal("second", reactions[surplusID])
		var envelope instagramRaw
		require.NoError(json.Unmarshal([]byte(raws[surplusID]), &envelope))
		assert.Nil(envelope.DerivedReactions)
		assert.Len(envelope.Candidates, 2)
		for i, candidate := range envelope.Candidates {
			assert.JSONEq(string(candidates[i]), string(candidate))
		}
	}
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
	for _, section := range []string{"message_requests", "spam"} {
		t.Run(section, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, root := testutil.NewTestStore(t), t.TempDir()
			instagramTestShard(t, root, "circle", "message_1.json", instagramTestRecord("hello"))
			thread := filepath.Join(root, "your_instagram_activity", "messages", section, "thread")
			require.NoError(os.MkdirAll(thread, 0700))
			require.NoError(os.WriteFile(filepath.Join(thread, "message_1.json"), []byte(`{"messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"excluded"}]}`), 0600))
			summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
			require.NoError(err)
			assert.True(summary.Incomplete)
			assert.Equal(int64(1), summary.FilesSkipped)
			assert.Equal(1, countMessages(t, st, ""))
			assert.Zero(countMessages(t, st, "snippet='excluded'"))
		})
	}
}

func TestInstagramSenderOrMediaChangesRetainMessages(t *testing.T) {
	for _, tc := range []struct {
		name, sender string
		media        bool
	}{
		{name: "rename", sender: "Renamed Example"},
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
			instagramTestShard(t, root, "circle", "message_1.json", record)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example", AttachmentsDir: t.TempDir()}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			ids := instagramIDs(t, st)
			record["sender_name"] = tc.sender
			if tc.media {
				record["photos"] = []map[string]string{{"uri": "photos/two.jpg"}}
			}
			instagramTestShard(t, root, "circle", "message_1.json", record)
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
	t.Run("nested key order", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st, root := testutil.NewTestStore(t), t.TempDir()
		record := instagramTestRecord("same")
		record["photos"] = []map[string]any{{"uri": "photo.jpg", "extra_one": "a", "extra_two": "b"}}
		filename := instagramTestShard(t, root, "circle", "message_1.json", record)
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		data, err := os.ReadFile(filename)
		require.NoError(err)
		before := `"extra_one":"a","extra_two":"b"`
		require.Contains(string(data), before)
		require.NoError(os.WriteFile(filename, []byte(strings.Replace(string(data), before, `"extra_two":"b","extra_one":"a"`, 1)), 0600))
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
		assert.Equal(1, countMessages(t, st, ""))
	})
	t.Run("changed reactions with extra fields", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st, root := testutil.NewTestStore(t), t.TempDir()
		a, b := instagramTestRecord("same"), instagramTestRecord("same")
		a["extra_field"], b["extra_field"] = "first", "second"
		a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "a"}}
		b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "b"}}
		instagramTestShard(t, root, "circle", "message_1.json", a, b)
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		_, beforeReactions := instagramSnapshot(t, st)
		require.Len(beforeReactions, 2)
		a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "c"}}
		b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "d"}}
		instagramTestShard(t, root, "circle", "message_1.json", b, a)
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.False(summary.Incomplete)
		assert.Zero(summary.MessagesAdded)
		_, reactions := instagramSnapshot(t, st)
		for id, reaction := range beforeReactions {
			if reaction == "a" {
				assert.Equal("c", reactions[id])
			} else {
				assert.Equal("b", reaction)
				assert.Equal("d", reactions[id])
			}
		}
	})
	for _, field := range []string{"photo timestamp", "video timestamp", "unknown field", "large identifier", "large media identifier", "nested field", "share field", "sticker field", "reaction field"} {
		t.Run(field, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			a, b := instagramTestRecord("same"), instagramTestRecord("same")
			if kind, ok := strings.CutSuffix(field, " timestamp"); ok {
				a[kind+"s"] = []map[string]any{{"uri": "media", "creation_timestamp": 100}}
				b[kind+"s"] = []map[string]any{{"uri": "media", "creation_timestamp": 200}}
			} else if field == "nested field" {
				a["photos"] = []map[string]any{{"uri": "photo.jpg", "extra_field": "first"}}
				b["photos"] = []map[string]any{{"uri": "photo.jpg", "extra_field": "second"}}
			} else if field == "share field" {
				a["share"] = map[string]any{"link": "https://example.org", "extra_field": "first"}
				b["share"] = map[string]any{"link": "https://example.org", "extra_field": "second"}
			} else if field == "sticker field" {
				a["sticker"] = map[string]any{"uri": "sticker.png", "extra_field": "first"}
				b["sticker"] = map[string]any{"uri": "sticker.png", "extra_field": "second"}
			} else if field == "reaction field" {
				a["reactions"] = []map[string]any{{"actor": "Nova Example", "reaction": "a", "extra_field": "first"}}
				b["reactions"] = []map[string]any{{"actor": "Nova Example", "reaction": "a", "extra_field": "second"}}
			} else if field == "large identifier" {
				a["extra_id"], b["extra_id"] = int64(9007199254740992), int64(9007199254740993)
			} else if field == "large media identifier" {
				a["photos"] = []map[string]any{{"uri": "photo.jpg", "extra_id": int64(9007199254740992)}}
				b["photos"] = []map[string]any{{"uri": "photo.jpg", "extra_id": int64(9007199254740993)}}
			} else {
				a["extra_field"], b["extra_field"] = "first", "second"
			}
			instagramTestShard(t, root, "circle", "message_1.json", a, b)
			opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			before, _ := instagramSnapshot(t, st)
			instagramTestShard(t, root, "circle", "message_1.json", b, a)
			_, err = ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			after, _ := instagramSnapshot(t, st)
			assert.Equal(before, after, "reordered replay")
			for _, record := range []map[string]any{b, a} {
				instagramTestShard(t, root, "circle", "message_1.json", record)
				_, err = ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				after, _ = instagramSnapshot(t, st)
				assert.Equal(before, after, "partial replay")
			}
		})
	}
	t.Run("reordered photos", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		record := instagramTestRecord("same")
		first := map[string]any{"uri": "a.jpg", "creation_timestamp": 100}
		second := map[string]any{"uri": "b.jpg", "creation_timestamp": 200}
		record["photos"] = []map[string]any{first, second}
		instagramTestShard(t, root, "circle", "message_1.json", record)
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		record["photos"] = []map[string]any{second, first}
		instagramTestShard(t, root, "circle", "message_1.json", record)
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
		assert.Equal(1, countMessages(t, st, ""))
	})
	t.Run("reordered", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		root := t.TempDir()
		a, b := instagramTestRecord("hello"), instagramTestRecord("hello")
		a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "✨"}}
		b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "❤️"}}
		c, d := instagramTestRecord("hello"), instagramTestRecord("hello")
		c["type"], d["type"] = "Generic", "Subscribe"
		e, f := instagramTestRecord("hello"), instagramTestRecord("hello")
		e["type"], f["type"], e["missed"], f["missed"] = "Call", "Call", true, true
		e["call_duration"], f["call_duration"] = 10, 20
		instagramTestShard(t, root, "circle", "message_1.json", a, b, c, d, e, f)
		opts := ImportOptions{Source: "instagram", RootDir: root, SelfName: "Nova Example", Account: "example"}
		_, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		beforeRaw, before := instagramSnapshot(t, st)
		instagramTestShard(t, root, "circle", "message_1.json", f, d, b)
		instagramTestShard(t, root, "circle", "message_2.json", e, c, a)
		_, err = ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM reactions`).Scan(&count))
		assert.Equal(2, count)
		afterRaw, after := instagramSnapshot(t, st)
		for id, raw := range afterRaw {
			afterRaw[id] = strings.ReplaceAll(raw, "message_2.json", "message_1.json")
		}
		assert.Equal(beforeRaw, afterRaw)
		assert.Equal(before, after)
		require.NoError(os.Remove(filepath.Join(root, filepath.FromSlash(instagramInbox), "circle", "message_2.json")))
		original := instagramIDs(t, st)
		instagramTestShard(t, root, "circle", "message_1.json", b)
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Zero(summary.MessagesAdded)
		assert.Equal(original, instagramIDs(t, st))
		newReaction := instagramTestRecord("hello")
		newReaction["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "c"}}
		instagramTestShard(t, root, "circle", "message_1.json", a, newReaction)
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
		raws, reactions := instagramSnapshot(t, st)
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
			afterRaw, afterReactions := instagramSnapshot(t, st)
			assert.Equal(reactions, afterReactions)
			assert.Equal(raws, afterRaw)
		}
	})

	t.Run("corrupted stored evidence", func(t *testing.T) {
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

func TestInstagramMixedMetadataReactionsPreserveArchivedCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  []string
	}{
		{"equal distinct", []string{"A", "A", "B"}},
		{"equal identical", []string{"A", "A", "A"}},
		{"partial distinct", []string{"A", "A", "B", "B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, root := testutil.NewTestStore(t), t.TempDir()
			var records []map[string]any
			for _, value := range tc.old {
				record := instagramTestRecord("same")
				record["extra_field"] = value
				records = append(records, record)
			}
			instagramTestShard(t, root, "circle", "message_1.json", records...)
			opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
			_, err := ImportDYI(t.Context(), st, opts)
			require.NoError(err)
			before, _ := instagramSnapshot(t, st)
			for i, record := range records[:3] {
				if i > 0 {
					record["extra_field"] = "C"
				}
				record["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "like"}}
			}
			instagramTestShard(t, root, "circle", "message_1.json", records[:3]...)
			for range 2 {
				summary, err := ImportDYI(t.Context(), st, opts)
				require.NoError(err)
				assert.True(summary.Incomplete)
				assert.Zero(summary.MessagesAdded)
				after, _ := instagramSnapshot(t, st)
				assert.Equal(before, after)
			}
		})
	}
}

func TestInstagramMixedMetadataKeepsUnambiguousMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	a, b := instagramTestRecord("same"), instagramTestRecord("same")
	a["extra_field"], b["extra_field"] = "A", "B"
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	before, _ := instagramSnapshot(t, st)
	b["extra_field"] = "C"
	a["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "like"}}
	b["reactions"] = []map[string]string{{"actor": "Nova Example", "reaction": "love"}}
	instagramTestShard(t, root, "circle", "message_1.json", a, b)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.False(summary.Incomplete)
	assert.Zero(summary.MessagesAdded)
	_, reactions := instagramSnapshot(t, st)
	for id, raw := range before {
		if strings.Contains(raw, `"extra_field":"A"`) {
			assert.Equal("like", reactions[id])
		} else {
			assert.Equal("love", reactions[id])
		}
	}
}

func TestInstagramReportsPossibleSenderDuplicates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	record := instagramTestRecord("same")
	record["sender_name"] = "Mira Example"
	instagramTestShard(t, root, "circle", "message_1.json", record)
	opts := ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"}
	_, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	before, _ := instagramSnapshot(t, st)
	record["sender_name"] = "Lumen Example"
	original := instagramTestRecord("same")
	instagramTestShard(t, root, "circle", "message_1.json", original, record)
	for i := range 2 {
		summary, err := ImportDYI(t.Context(), st, opts)
		require.NoError(err)
		assert.Equal(1+i, summary.PossibleDuplicates)
		assert.Equal(int64(1-i), summary.MessagesAdded)
		after, _ := instagramSnapshot(t, st)
		assert.Len(after, 2)
		for id, raw := range before {
			assert.Equal(raw, after[id])
		}
		var senders int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(DISTINCT sender_id) FROM messages`).Scan(&senders))
		assert.Equal(2, senders)
	}
	other := instagramTestRecord("same")
	other["sender_name"] = "Another Example"
	instagramTestShard(t, root, "other", "message_1.json", other)
	later := instagramTestRecord("same")
	later["sender_name"], later["timestamp_ms"] = "Another Example", int64(1730500001123)
	instagramTestShard(t, root, "circle", "message_1.json", later)
	summary, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.PossibleDuplicates)
	assert.Equal(4, countMessages(t, st, ""))
}

func TestInstagramSkipsOverflowShardAndImportsOtherThreads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, root := testutil.NewTestStore(t), t.TempDir()
	instagramTestShard(t, root, "a", "message_999999999999999999999.json", instagramTestRecord("unsupported"))
	instagramTestShard(t, root, "b", "message_1.json", instagramTestRecord("valid"))
	instagramTestShard(t, root, "b", "message_999999999999999999999.json", instagramTestRecord("also unsupported"))
	summary, err := ImportDYI(t.Context(), st, ImportOptions{Source: "instagram", RootDir: root, Account: "example", SelfName: "Nova Example"})
	require.NoError(err)
	assert.Equal(int64(2), summary.FilesSkipped)
	assert.True(summary.Incomplete)
	assert.Equal(1, countMessages(t, st, ""))
}
