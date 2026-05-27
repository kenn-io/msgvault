package fbmessenger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func importFixture(t *testing.T, st *store.Store, rootDir string, extra ...func(*ImportOptions)) *ImportSummary {
	t.Helper()
	opts := ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        rootDir,
		Format:         "auto",
		AttachmentsDir: t.TempDir(),
	}
	for _, f := range extra {
		f(&opts)
	}
	summary, err := ImportDYI(context.Background(), st, opts)
	requirepkg.NoError(t, err, "ImportDYI(%s)", rootDir)
	return summary
}

func countMessages(t *testing.T, st *store.Store, where string, args ...any) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM messages"
	if where != "" {
		q += " WHERE " + where
	}
	err := st.DB().QueryRow(q, args...).Scan(&n)
	requirepkg.NoError(t, err, "count query")
	return n
}

func TestImportDYI_JSONSimple(t *testing.T) {
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	summary := importFixture(t, st, "testdata/json_simple")
	// json_simple has 1 inbox thread (3 messages) + 1 archived thread (1 message) = 4
	assert.Equal(int64(4), summary.MessagesAdded, "MessagesAdded")
	assert.False(summary.HardErrors, "HardErrors")
	assert.Equal(4, countMessages(t, st, "message_type='fbmessenger'"), "messages count")
	assert.Equal(4, countMessages(t, st, "message_type='fbmessenger' AND sent_at IS NOT NULL"), "sent_at NULL rows exist")
	// Exactly one message_type present.
	rows, err := st.DB().Query("SELECT DISTINCT message_type FROM messages")
	requirepkg.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var types []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		types = append(types, s)
	}
	assert.Equal([]string{"fbmessenger"}, types)
}

// TestImportDYI_MojibakeRepaired verifies mojibake repair on the body
// stored in message_bodies independently of FTS5. The FTS5 MATCH
// assertion lives in importer_fts_test.go gated on the fts5 build tag.
func TestImportDYI_MojibakeRepaired(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	var body string
	err := st.DB().QueryRow(
		`SELECT body_text FROM message_bodies WHERE body_text LIKE '%café%'`,
	).Scan(&body)
	requirepkg.NoError(t, err, "body query")
	assertpkg.Contains(t, body, "café")
}

func TestImportDYI_DirectChat(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	var ct string
	err := st.DB().QueryRow(
		"SELECT conversation_type FROM conversations WHERE source_conversation_id='inbox/alice_ABC123'",
	).Scan(&ct)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, "direct_chat", ct, "conv type")
}

func TestImportDYI_GroupChat(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_group")
	var ct string
	err := st.DB().QueryRow(
		"SELECT conversation_type FROM conversations WHERE source_conversation_id='inbox/crew_GRP123'",
	).Scan(&ct)
	require.NoError(err)
	assert.Equal("group_chat", ct, "conv type")
	// Three facebook.messenger participants (Taylor/Alice/Bob) plus the
	// self seed. The self seed and the slug-derived sender address match
	// ("test.user@facebook.messenger"), so they collapse to one row.
	var n int
	err = st.DB().QueryRow(
		"SELECT COUNT(*) FROM participants WHERE domain='facebook.messenger'",
	).Scan(&n)
	require.NoError(err)
	assert.GreaterOrEqual(n, 3, "participants(fb)")
	// Every message has at least one 'to' recipient.
	var badMsgs int
	err = st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.conversation_id = (SELECT id FROM conversations WHERE source_conversation_id='inbox/crew_GRP123')
		AND NOT EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id = m.id AND r.recipient_type='to')
	`).Scan(&badMsgs)
	require.NoError(err)
	assert.Equal(0, badMsgs, "messages without 'to' recipients")
}

func TestImportDYI_MultifileNumericSort(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_multifile")
	rows, err := st.DB().Query(`
		SELECT source_message_id, sent_at FROM messages
		WHERE source_id = (SELECT id FROM sources WHERE source_type='facebook_messenger')
		ORDER BY sent_at ASC
	`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var ids []string
	var lastTime time.Time
	for rows.Next() {
		var id string
		var sentAt sql.NullTime
		require.NoError(rows.Scan(&id, &sentAt))
		if sentAt.Valid {
			assert.True(sentAt.Time.After(lastTime), "non-monotonic sent_at at %s", id)
			lastTime = sentAt.Time
		}
		ids = append(ids, id)
	}
	require.Len(ids, 4)
	// All source_message_id values must be prefixed dave_MULTI__ and
	// have monotonic index suffixes.
	for i, id := range ids {
		want := fmt.Sprintf("inbox/dave_MULTI__%d", i)
		assert.Equal(want, id, "source_message_id[%d]", i)
	}
}

func TestImportDYI_Idempotent(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	before := snapshotRowCounts(t, st)
	_ = importFixture(t, st, "testdata/json_simple")
	after := snapshotRowCounts(t, st)
	for k, v := range before {
		assertpkg.Equal(t, v, after[k], "%s", k)
	}
}

func snapshotRowCounts(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	out := make(map[string]int)
	for _, tbl := range []string{"messages", "participants", "message_recipients", "attachments", "reactions", "conversations", "labels"} {
		var n int
		err := st.DB().QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n)
		requirepkg.NoError(t, err, "count %s", tbl)
		out[tbl] = n
	}
	return out
}

// A thread containing both a valid numbered file and an unrecognized
// sibling (e.g. message_final.json) must import the valid file and
// report the bad sibling via MessagesSkipped rather than aborting the
// entire thread.
func TestImportDYI_UnnumberedSiblingSkipped(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "mixnames_OK")
	require.NoError(os.MkdirAll(threadPath, 0755))
	good := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","content":"good message"}
],"title":"mix"}`
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(good), 0644))
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_final.json"), []byte(`not json`), 0644))
	summary := importFixture(t, st, tmp)
	assert.False(summary.HardErrors, "HardErrors")
	assert.Equal(int64(1), summary.MessagesAdded, "MessagesAdded")
	assert.GreaterOrEqual(summary.FilesSkipped, int64(1), "FilesSkipped (bad sibling)")
	assert.Equal(int64(0), summary.MessagesSkipped, "MessagesSkipped (no message was rejected)")
	assert.Equal(int64(0), summary.ThreadsSkipped, "ThreadsSkipped")
	// Valid file must be imported.
	var n int
	err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/mixnames_OK'",
	).Scan(&n)
	require.NoError(err)
	assert.Equal(1, n, "conversation not imported")
}

func TestImportDYI_CorruptSkipped(t *testing.T) {
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	summary := importFixture(t, st, "testdata/corrupt")
	assert.False(summary.HardErrors, "HardErrors")
	assert.GreaterOrEqual(summary.ThreadsSkipped, int64(1), "ThreadsSkipped (corrupt thread)")
	assert.Equal(int64(0), summary.MessagesSkipped, "MessagesSkipped (only whole-thread skip)")
	// Good sibling message must still be imported.
	var n int
	err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/goodsibling_OK'",
	).Scan(&n)
	requirepkg.NoError(t, err)
	assert.Equal(1, n, "good sibling not imported")
}

func TestImportDYI_AttachmentStorage(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	attachDir := t.TempDir()
	opts := ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        "testdata/json_with_media",
		Format:         "auto",
		AttachmentsDir: attachDir,
	}
	_, err := ImportDYI(context.Background(), st, opts)
	require.NoError(err)
	// Compute expected hash from fixture.
	png, err := os.ReadFile("testdata/json_with_media/your_activity_across_facebook/messages/inbox/bob_XYZ789/photos/tiny.png")
	require.NoError(err)
	wantHash := fmt.Sprintf("%x", sha256.Sum256(png))

	var contentHash, storagePath string
	var size int64
	err = st.DB().QueryRow(
		"SELECT content_hash, storage_path, size FROM attachments LIMIT 1",
	).Scan(&contentHash, &storagePath, &size)
	require.NoError(err)
	assert.Equal(wantHash, contentHash, "content_hash")
	assert.NotEmpty(storagePath, "storage_path")
	absStorage := filepath.Join(attachDir, storagePath)
	got, err := os.ReadFile(absStorage)
	require.NoError(err, "stored file")
	assert.Equal(string(png), string(got), "stored bytes")
	assert.Equal(int64(len(png)), size, "size")
}

func TestImportDYI_AttachmentPathEscapeRejected(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	// Build a fixture whose JSON references ../../etc/passwd.
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "evil_ESC")
	require.NoError(os.MkdirAll(threadPath, 0755))
	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"../../etc/passwd"}]}
],"title":"x"}`
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644))

	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.False(summary.HardErrors, "HardErrors")
	// Exactly one attachment row, with empty storage_path and content_hash.
	var sp, ch string
	err = st.DB().QueryRow("SELECT storage_path, content_hash FROM attachments LIMIT 1").Scan(&sp, &ch)
	require.NoError(err)
	assert.Empty(sp, "storage_path: path escape not rejected")
	assert.Empty(ch, "content_hash: path escape not rejected")
}

// TestImportDYI_AttachmentSymlinkRejected verifies that an attachment URI
// pointing at a symlink (e.g. a malicious DYI export that planted a
// symlink to a sensitive local file) is not followed: handleAttachment
// returns no storage_path/content_hash, so the symlink target is never
// copied into the attachment store.
func TestImportDYI_AttachmentSymlinkRejected(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "evil_LNK")
	photosDir := filepath.Join(threadPath, "photos")
	require.NoError(os.MkdirAll(photosDir, 0755))
	// Create a "secret" file outside the attachment URI tree and a
	// symlink at the URI path that points at it. The URI itself stays
	// inside the export root, so the path-escape guard does not catch
	// it; only the symlink check does.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(os.WriteFile(secret, []byte("password=hunter2"), 0600))
	link := filepath.Join(photosDir, "innocent.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"messages/inbox/evil_LNK/photos/innocent.png"}]}
],"title":"x"}`
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644))

	attachmentsDir := t.TempDir()
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: attachmentsDir,
	})
	require.NoError(err)
	assert.False(summary.HardErrors, "HardErrors")
	var sp, ch string
	err = st.DB().QueryRow("SELECT storage_path, content_hash FROM attachments LIMIT 1").Scan(&sp, &ch)
	require.NoError(err)
	assert.Empty(sp, "storage_path: symlinked attachment not rejected")
	assert.Empty(ch, "content_hash: symlinked attachment not rejected")
	// Defense in depth: assert nothing under attachmentsDir contains the
	// secret bytes, so even a future copy regression would be caught.
	_ = filepath.Walk(attachmentsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		assert.NotContains(string(data), "hunter2", "symlink target leaked into attachments store at %s", p)
		return nil
	})
}

func TestImportDYI_MissingAttachment(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "missing_MIS")
	require.NoError(os.MkdirAll(threadPath, 0755))
	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"messages/inbox/missing_MIS/photos/gone.png"}]}
],"title":"x"}`
	require.NoError(os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644))
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.False(summary.HardErrors, "HardErrors")
	var sp, ch string
	err = st.DB().QueryRow("SELECT storage_path, content_hash FROM attachments LIMIT 1").Scan(&sp, &ch)
	require.NoError(err)
	assert.Empty(sp, "storage_path: missing attachment should have empty storage_path")
	assert.Empty(ch, "content_hash: missing attachment should have empty content_hash")
}

// TestImportDYI_ReactionsFirstClass verifies reaction rows and the
// "[reacted: ...]" body-append independently of FTS5. The FTS5 MATCH
// half of the dual-path lives in importer_fts_test.go.
func TestImportDYI_ReactionsFirstClass(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	var n int
	err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM reactions r
		JOIN message_bodies b ON b.message_id = r.message_id
		WHERE b.body_text LIKE '%café%'
	`).Scan(&n)
	require.NoError(err)
	assert.Equal(2, n, "reactions")
	var bodyCount int
	err = st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_bodies WHERE body_text LIKE '%[reacted:%'`,
	).Scan(&bodyCount)
	require.NoError(err)
	assert.GreaterOrEqual(bodyCount, 1, "body with [reacted: suffix")
}

func TestImportDYI_NonTextMessageBodies(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_nontext")
	want := map[string]string{
		"inbox/sam_NONTXT__0": "[system] Sam left the chat",
		"inbox/sam_NONTXT__1": "[shared link] https://example.com/article\nExample share text",
		"inbox/sam_NONTXT__2": "[call: missed, 0s]",
		"inbox/sam_NONTXT__3": "[call: 3m 12s]",
		"inbox/sam_NONTXT__4": "[photo]",
		"inbox/sam_NONTXT__5": "[sticker]",
	}
	for id, wantBody := range want {
		var body string
		err := st.DB().QueryRow(st.Rebind(`
			SELECT b.body_text FROM message_bodies b
			JOIN messages m ON m.id = b.message_id
			WHERE m.source_message_id = ?`), id).Scan(&body)
		if !assertpkg.NoError(t, err, "%s", id) {
			continue
		}
		assertpkg.Equal(t, wantBody, body, "%s body", id)
	}
}

func TestImportDYI_MixedFormatJSONWins(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/mixed")
	// Exactly one conversation.
	var n int
	err := st.DB().QueryRow("SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/eve_MIX'").Scan(&n)
	require.NoError(err)
	assert.Equal(1, n, "conversations")
	// 2 messages, no __html_ prefix.
	err = st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n)
	require.NoError(err)
	assert.Equal(2, n, "messages")
	err = st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_message_id LIKE '%html_%'").Scan(&n)
	require.NoError(err)
	assert.Equal(0, n, "html_ prefixed rows")
}

func TestImportDYI_FormatBoth(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        "testdata/mixed",
		Format:         "both",
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.False(summary.HardErrors, "HardErrors")
	var n int
	err = st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n)
	require.NoError(err)
	assert.Equal(4, n, "messages")
	err = st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_message_id LIKE '%__html_%'").Scan(&n)
	require.NoError(err)
	assert.Equal(2, n, "html rows")
	// One conversation row, not two.
	err = st.DB().QueryRow("SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/eve_MIX'").Scan(&n)
	require.NoError(err)
	assert.Equal(1, n, "conversations")
}

func TestImportDYI_IsFromMe(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	_, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        "testdata/json_simple",
		Format:         "auto",
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	var ident string
	err = st.DB().QueryRow(
		"SELECT identifier FROM sources WHERE source_type='facebook_messenger'",
	).Scan(&ident)
	require.NoError(err)
	assert.Equal("test.user@facebook.messenger", ident, "identifier")
	// Messages authored by Test User should have is_from_me=1.
	var wesFromMe, aliceFromMe int
	err = st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.is_from_me IS TRUE AND m.source_message_id LIKE 'inbox/alice_ABC123__%'
	`).Scan(&wesFromMe)
	require.NoError(err)
	assert.GreaterOrEqual(wesFromMe, 1, "wes is_from_me rows")
	err = st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.is_from_me IS NOT TRUE AND m.source_message_id LIKE 'inbox/alice_ABC123__%'
	`).Scan(&aliceFromMe)
	require.NoError(err)
	assert.GreaterOrEqual(aliceFromMe, 1, "alice is_from_me=0 rows")
}

func TestImportDYI_LabelTaxonomy(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	// Messenger and Messenger / Inbox and Messenger / Archived must exist.
	for _, name := range []string{"Messenger", "Messenger / Inbox", "Messenger / Archived"} {
		var n int
		err := st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM labels WHERE name = ?"), name).Scan(&n)
		require.NoError(err)
		assert.Equal(1, n, "label %q count", name)
	}
	// Every inbox message has both Messenger and Messenger / Inbox labels.
	var n int
	err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		JOIN messages m ON m.id = ml.message_id
		WHERE l.name = 'Messenger / Inbox'
		AND m.source_message_id LIKE 'inbox/alice_ABC123__%'
	`).Scan(&n)
	require.NoError(err)
	assert.Equal(3, n, "inbox labels on alice msgs")
	err = st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		JOIN messages m ON m.id = ml.message_id
		WHERE l.name = 'Messenger / Archived'
		AND m.source_message_id LIKE 'archived_threads/zoe_ARCH__%'
	`).Scan(&n)
	require.NoError(err)
	assert.Equal(1, n, "archived labels on zoe msgs")
}

func TestImportDYI_SelfParticipantSeeded(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	// Empty DYI tree with just messages/inbox/.
	require.NoError(os.MkdirAll(filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox"), 0755))
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.Equal(int64(0), summary.MessagesProcessed, "MessagesProcessed")
	assert.False(summary.HardErrors, "HardErrors")
	var n int
	err = st.DB().QueryRow(
		st.Rebind("SELECT COUNT(*) FROM participants WHERE email_address = ? AND domain = 'facebook.messenger'"),
		"test.user@facebook.messenger",
	).Scan(&n)
	require.NoError(err)
	assert.Equal(1, n, "self participant count")
}

func TestImportDYI_MeDomainValidation(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	_, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "wes@gmail.com",
		RootDir:        "testdata/json_simple",
		AttachmentsDir: t.TempDir(),
	})
	require.Error(err)
	assert.Contains(err.Error(), "facebook.messenger", "error should mention facebook.messenger")
	var n int
	err = st.DB().QueryRow(
		"SELECT COUNT(*) FROM sources WHERE source_type='facebook_messenger'",
	).Scan(&n)
	require.NoError(err)
	assert.Equal(0, n, "sources")
}

// largeFixtureSize is the number of messages in the timing-tripwire
// fixture. Sized to be fast enough to always run (including under
// `go test -short`) while still catching catastrophic regressions.
const largeFixtureSize = 150

// Procedurally-generated fixture for the timing tripwire.
func writeLargeFixture(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "big_BIG")
	requirepkg.NoError(t, os.MkdirAll(threadPath, 0755))
	type rawMsg struct {
		SenderName  string `json:"sender_name"`
		TimestampMs int64  `json:"timestamp_ms"`
		Content     string `json:"content"`
		Type        string `json:"type"`
	}
	type rawPart struct {
		Name string `json:"name"`
	}
	type rawExport struct {
		Participants []rawPart `json:"participants"`
		Messages     []rawMsg  `json:"messages"`
		Title        string    `json:"title"`
	}
	exp := rawExport{
		Participants: []rawPart{{Name: "Test User"}, {Name: "Big Friend"}},
		Title:        "Big Friend",
	}
	for i := range largeFixtureSize {
		sender := "Test User"
		if i%2 == 1 {
			sender = "Big Friend"
		}
		exp.Messages = append(exp.Messages, rawMsg{
			SenderName:  sender,
			TimestampMs: 1600000000000 + int64(i)*60000,
			Content:     fmt.Sprintf("Message %d", i),
			Type:        "Generic",
		})
	}
	data, err := json.Marshal(exp)
	requirepkg.NoError(t, err)
	requirepkg.NoError(t, os.WriteFile(filepath.Join(threadPath, "message_1.json"), data, 0644))
	return tmp
}

// writeMultiThreadFixture lays out `n` inbox threads under a DYI root
// at tmp, each with a single short message. Threads are named
// "thread_{i}_OK" so that Discover sorts them deterministically.
func writeMultiThreadFixture(t *testing.T, n int) string {
	t.Helper()
	tmp := t.TempDir()
	for i := range n {
		name := fmt.Sprintf("thread_%02d_OK", i)
		threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", name)
		requirepkg.NoError(t, os.MkdirAll(threadPath, 0755))
		body := fmt.Sprintf(
			`{"participants":[{"name":"Test User"},{"name":"Friend %d"}],"messages":[`+
				`{"sender_name":"Friend %d","timestamp_ms":%d,"type":"Generic","content":"hello from %d"}`+
				`],"title":"Friend %d"}`,
			i, i, 1600000000000+int64(i)*60000, i, i,
		)
		requirepkg.NoError(t, os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644))
	}
	return tmp
}

// TestImportDYI_ResumeFromCheckpoint seeds an active sync with a prior
// fbmessengerCheckpoint pointing past the first thread, then runs
// ImportDYI and verifies that (a) WasResumed is true and (b) the
// already-processed thread is skipped on the second run (while still
// present in the store from the first run so idempotence holds).
func TestImportDYI_ResumeFromCheckpoint(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 3)

	// First run: import everything. This populates the DB and leaves
	// no checkpoint (CompleteSync clears cursor_before in practice
	// because we only read when there's an *active* run; a completed
	// run is fine to coexist).
	first := importFixture(t, st, root)
	assert.False(first.WasResumed, "first run WasResumed")
	require.Equal(int64(3), first.MessagesAdded, "first run MessagesAdded")
	require.Equal(3, first.ThreadsProcessed, "first run ThreadsProcessed")

	// Simulate an in-progress run: create a new running sync_run for
	// the facebook_messenger source and write a fbmessengerCheckpoint
	// whose ThreadIndex == 2 (two threads already done).
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "import-messenger")
	require.NoError(err)
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:          absRoot,
		ThreadIndex:      2,
		LastMessageIndex: 0,
	})
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         string(cpJSON),
		MessagesProcessed: 2,
		MessagesAdded:     2,
	}))

	// Second run: should detect the active checkpoint and resume,
	// processing only the 3rd thread.
	before := snapshotRowCounts(t, st)
	second := importFixture(t, st, root)
	after := snapshotRowCounts(t, st)

	assert.True(second.WasResumed, "second run WasResumed")
	assert.Equal(1, second.ThreadsProcessed, "second run ThreadsProcessed (only last thread)")
	// Idempotence: row counts must not change (source_message_id
	// dedupes the one re-imported thread if it were processed; but
	// here the resume skip means it is not touched at all).
	for k, v := range before {
		assert.Equal(v, after[k], "%s", k)
	}
	// All three threads must still be present.
	var n int
	err = st.DB().QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE source_conversation_id LIKE 'inbox/thread_%_OK'`,
	).Scan(&n)
	require.NoError(err)
	assert.Equal(3, n, "conversations")
}

// TestImportDYI_ResumeWrongRootRejected verifies that a prior
// checkpoint for a different RootDir is rejected.
func TestImportDYI_ResumeWrongRootRejected(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 2)

	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "import-messenger")
	require.NoError(err)
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:     "/some/other/dir",
		ThreadIndex: 1,
	})
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken: string(cpJSON),
	}))

	_, err = ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        root,
		AttachmentsDir: t.TempDir(),
	})
	require.Error(err, "expected error for wrong root")
	assertpkg.Contains(t, err.Error(), "different root")
}

// TestImportDYI_ResumeFromFailedSync verifies that a checkpoint saved
// before FailSync is still found on the next run, so interrupted imports
// can resume instead of restarting from scratch.
func TestImportDYI_ResumeFromFailedSync(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 3)

	// First run: import everything.
	first := importFixture(t, st, root)
	require.Equal(int64(3), first.MessagesAdded, "first run MessagesAdded")

	// Simulate a failed (interrupted) sync: create a sync run, save a
	// checkpoint, then mark it failed — mimicking what happens when the
	// user hits Ctrl-C.
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "import-messenger")
	require.NoError(err)
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:     absRoot,
		ThreadIndex: 2,
	})
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         string(cpJSON),
		MessagesProcessed: 2,
		MessagesAdded:     2,
	}))
	// Mark the sync as failed, simulating a graceful interrupt.
	require.NoError(st.FailSync(syncID, "context canceled"))

	// The next run must find the failed sync's checkpoint and resume.
	second := importFixture(t, st, root)
	assert.True(second.WasResumed, "second run WasResumed")
	assert.Equal(1, second.ThreadsProcessed, "second run ThreadsProcessed (only last thread)")
}

// TestImportDYI_ResumeFromFirstThreadCheckpoint verifies that a
// checkpoint saved while still processing the first thread
// (ThreadIndex == 0) is treated as resumable rather than ignored.
// source_message_id dedup covers the data path; this test asserts the
// UX-visible state (WasResumed true and cumulative counters carried
// forward) so a user-visible interrupt during thread 0 is reflected in
// the next run's summary.
func TestImportDYI_ResumeFromFirstThreadCheckpoint(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 2)

	// Seed a failed sync whose checkpoint is mid-first-thread.
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "import-messenger")
	require.NoError(err)
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:          absRoot,
		ThreadIndex:      0,
		LastMessageIndex: 0,
	})
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         string(cpJSON),
		MessagesProcessed: 1,
		MessagesAdded:     1,
	}))
	require.NoError(st.FailSync(syncID, "context canceled"))

	summary := importFixture(t, st, root)
	assert.True(summary.WasResumed, "WasResumed should be true for first-thread checkpoint")
	// Cumulative counters must carry over from the prior run.
	assert.GreaterOrEqual(summary.MessagesProcessed, int64(1),
		"MessagesProcessed should carry-over from prior run")
}

func TestImportDYI_InvalidFormatRejected(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 1)
	_, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        root,
		AttachmentsDir: t.TempDir(),
		Format:         "jsno",
	})
	requirepkg.Error(t, err, "expected error for invalid format")
	assertpkg.Contains(t, err.Error(), "unknown --format")
}

// TestImportDYI_StaleFailedCheckpointIgnoredAfterCompletion verifies that a
// failed run's checkpoint is not used to resume a later import once a
// successful run has occurred since. Without the fix,
// GetLatestCheckpointedSync would still return the older failed run because
// its status filter (running/failed) excluded the more recent completed
// run, and a re-import would silently resume from the stale checkpoint
// and skip threads already covered by the successful run.
func TestImportDYI_StaleFailedCheckpointIgnoredAfterCompletion(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 3)

	// Seed a failed sync with a checkpoint pointing past thread 0.
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	require.NoError(err)
	failID, err := st.StartSync(src.ID, "import-messenger")
	require.NoError(err)
	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{RootDir: absRoot, ThreadIndex: 2})
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(failID, &store.Checkpoint{
		PageToken: string(cpJSON), MessagesProcessed: 2, MessagesAdded: 2,
	}))
	require.NoError(st.FailSync(failID, "context canceled"))

	// Run a successful import after the failed run. This becomes the
	// latest sync, so a future re-import must NOT resume from the older
	// failed checkpoint.
	first := importFixture(t, st, root)
	require.Equal(int64(3), first.MessagesAdded, "first run MessagesAdded")

	second := importFixture(t, st, root)
	assert.False(second.WasResumed,
		"stale failed checkpoint resumed despite later completed run")
	assert.Equal(3, second.ThreadsProcessed,
		"ThreadsProcessed (full re-scan)")
}

// TestImportDYI_ReimportPicksUpNewMessages verifies that re-importing a root
// after a successful import picks up new messages added to an existing thread,
// rather than treating the completed run as resumable and skipping threads.
// Regression test for: GetLatestCheckpointedSync matching completed runs.
func TestImportDYI_ReimportPicksUpNewMessages(t *testing.T) {
	require := requirepkg.New(t)
	// Copy json_simple fixture to a temp dir so we can mutate it.
	root := t.TempDir()
	cpDir(t, "testdata/json_simple", root)

	st := testutil.NewTestStore(t)

	// First import: 4 messages (3 inbox + 1 archived).
	s1 := importFixture(t, st, root)
	require.Equal(int64(4), s1.MessagesAdded, "first import: MessagesAdded")
	before := countMessages(t, st, "message_type='fbmessenger'")
	require.Equal(4, before, "messages after first import")

	// Add a new message to the existing alice thread.
	threadFile := filepath.Join(root, "your_activity_across_facebook/messages/inbox/alice_ABC123/message_1.json")
	raw, err := os.ReadFile(threadFile)
	require.NoError(err)
	var thread map[string]any
	require.NoError(json.Unmarshal(raw, &thread))
	msgs := thread["messages"].([]any)
	newMsg := map[string]any{
		"sender_name":  "Alice Example",
		"timestamp_ms": float64(1600000200000),
		"content":      "New message after first import",
		"type":         "Generic",
	}
	thread["messages"] = append([]any{newMsg}, msgs...)
	updated, err := json.MarshalIndent(thread, "", "  ")
	require.NoError(err)
	require.NoError(os.WriteFile(threadFile, updated, 0o644))

	// Re-import the same root. The new message must be picked up.
	s2 := importFixture(t, st, root)
	after := countMessages(t, st, "message_type='fbmessenger'")
	assertpkg.Equal(t, before+1, after, "messages after re-import (added=%d)", s2.MessagesAdded)
}

// cpDir recursively copies src into dst.
func cpDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	requirepkg.NoError(t, err)
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			requirepkg.NoError(t, os.MkdirAll(dp, 0o755))
			cpDir(t, sp, dp)
		} else {
			data, err := os.ReadFile(sp)
			requirepkg.NoError(t, err)
			requirepkg.NoError(t, os.WriteFile(dp, data, 0o644))
		}
	}
}

// TestImportDYI_SynthesizedSenderLinkedToConversation verifies that when a
// message's sender_name is not in the thread's participants array (a
// system/orphan sender), the synthesized participant is still linked to the
// conversation via conversation_participants. Regression for the case where
// senderID was recorded on the message but not joined to the conversation,
// skewing participant-based analytics.
func TestImportDYI_SynthesizedSenderLinkedToConversation(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_ORPH")
	require.NoError(os.MkdirAll(threadDir, 0o755))
	fixture := map[string]any{
		"participants": []map[string]any{
			{"name": "Test User"},
			{"name": "Alice Example"},
		},
		"messages": []map[string]any{
			{
				"sender_name":  "Alice Example",
				"timestamp_ms": 1600000000000,
				"content":      "hi",
				"type":         "Generic",
			},
			{
				// Orphan sender: not in participants.
				"sender_name":  "Facebook User",
				"timestamp_ms": 1600000001000,
				"content":      "system message",
				"type":         "Generic",
			},
		},
		"title":                "Alice Example",
		"is_still_participant": true,
		"thread_type":          "Regular",
		"thread_path":          "inbox/alice_ORPH",
	}
	data, err := json.Marshal(fixture)
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(threadDir, "message_1.json"), data, 0o644))
	_, err = ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)

	// The synthesized "Facebook User" sender must be linked to the
	// conversation via conversation_participants, not just present as
	// sender_id on its message.
	var n int
	err = st.DB().QueryRow(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = (
			SELECT id FROM conversations WHERE source_conversation_id = 'inbox/alice_ORPH'
		)
		AND p.email_address = 'facebook.user@facebook.messenger'
	`).Scan(&n)
	require.NoError(err)
	assertpkg.Equal(t, 1, n, "orphan sender not linked to conversation")
}

// TestImportDYI_SenderIDPreservedOnReimport verifies that re-importing a
// thread whose message sender no longer resolves (e.g. the participant
// was renamed or the second import uses a different fixture) does not
// null-out the previously-recorded messages.sender_id, display name, or
// is_from_me flag. The importer reads any existing sender data and reuses
// it when the current run can't produce one.
func TestImportDYI_SenderIDPreservedOnReimport(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_PRES")
	require.NoError(os.MkdirAll(threadDir, 0o755))

	write := func(msg0Sender, msg1Sender string) {
		fixture := map[string]any{
			"participants": []map[string]any{
				{"name": "Test User"},
				{"name": "Alice Example"},
			},
			"messages": []map[string]any{
				{
					"sender_name":  msg0Sender,
					"timestamp_ms": 1600000000000,
					"content":      "from alice",
					"type":         "Generic",
				},
				{
					"sender_name":  msg1Sender,
					"timestamp_ms": 1600000001000,
					"content":      "from me",
					"type":         "Generic",
				},
			},
			"title":                "Alice Example",
			"is_still_participant": true,
			"thread_type":          "Regular",
			"thread_path":          "inbox/alice_PRES",
		}
		data, err := json.Marshal(fixture)
		require.NoError(err)
		require.NoError(os.WriteFile(filepath.Join(threadDir, "message_1.json"), data, 0o644))
	}

	// First import: message 0 from Alice, message 1 from Test User (self).
	write("Alice Example", "Test User")
	_, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)

	type snap struct {
		senderID sql.NullInt64
		isFromMe bool
		fromName sql.NullString
		fromPID  sql.NullInt64
	}
	capture := func(srcMsgID string) snap {
		t.Helper()
		var s snap
		err := st.DB().QueryRow(
			st.Rebind(`SELECT sender_id, is_from_me FROM messages WHERE source_message_id = ?`),
			srcMsgID,
		).Scan(&s.senderID, &s.isFromMe)
		require.NoError(err, "messages row for %s", srcMsgID)
		err = st.DB().QueryRow(st.Rebind(`
			SELECT mr.display_name, mr.participant_id
			FROM message_recipients mr
			JOIN messages m ON m.id = mr.message_id
			WHERE m.source_message_id = ? AND mr.recipient_type = 'from'
		`), srcMsgID).Scan(&s.fromName, &s.fromPID)
		require.NoError(err, "from recipient for %s", srcMsgID)
		return s
	}

	aliceBefore := capture("inbox/alice_PRES__0")
	selfBefore := capture("inbox/alice_PRES__1")
	require.True(aliceBefore.senderID.Valid, "alice msg setup: senderID")
	require.False(aliceBefore.isFromMe, "alice msg setup: isFromMe")
	require.True(selfBefore.senderID.Valid, "self msg setup: senderID")
	require.True(selfBefore.isFromMe, "self msg setup: isFromMe")

	// Second import: both sender_names stripped so the current run can't
	// resolve them. The importer must preserve prior sender_id, is_from_me,
	// and message_recipients for both messages.
	write("", "")
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	// Rehydrated self-authored messages must still count toward
	// FromMeCount so the CLI doesn't warn about a --me mismatch.
	assert.GreaterOrEqual(summary.FromMeCount, int64(1), "FromMeCount on rehydration")

	aliceAfter := capture("inbox/alice_PRES__0")
	selfAfter := capture("inbox/alice_PRES__1")

	assert.True(aliceAfter.senderID.Valid, "alice sender_id not preserved")
	assert.Equal(aliceBefore.senderID.Int64, aliceAfter.senderID.Int64, "alice sender_id not preserved")
	assert.False(aliceAfter.isFromMe, "alice is_from_me flipped to true")
	assert.True(aliceAfter.fromName.Valid)
	assert.Equal("Alice Example", aliceAfter.fromName.String, "alice from display_name")
	assert.True(aliceAfter.fromPID.Valid, "alice from participant_id valid")
	assert.Equal(aliceBefore.senderID.Int64, aliceAfter.fromPID.Int64, "alice from participant_id not preserved")

	assert.True(selfAfter.senderID.Valid, "self sender_id not preserved")
	assert.Equal(selfBefore.senderID.Int64, selfAfter.senderID.Int64, "self sender_id not preserved")
	assert.True(selfAfter.isFromMe, "self is_from_me not preserved (flipped to false)")
	// The self participant is seeded with an empty participants.display_name,
	// so rehydration must fall back to the prior message_recipients display
	// name rather than clobbering it with "".
	assert.True(selfAfter.fromName.Valid)
	assert.Equal("Test User", selfAfter.fromName.String, "self from display_name")

	// The account owner must NOT appear in "to" for the self-authored
	// message — otherwise the dropped is_from_me flag would inflate
	// participant analytics and cause self-to-self recipient rows.
	var selfInTo int
	err = st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = 'inbox/alice_PRES__1'
		  AND mr.recipient_type = 'to'
		  AND mr.participant_id = ?
	`), selfBefore.senderID.Int64).Scan(&selfInTo)
	require.NoError(err)
	assert.Equal(0, selfInTo, "self participant appeared in 'to' for self-authored message")
}

// TestImportDYI_ReimportRepairsConversationParticipant verifies that a
// conversation missing a participant row (the pre-fix state for databases
// imported before synthesized senders were linked) gets re-linked on a
// subsequent import via the sender_id-preservation rehydration path.
func TestImportDYI_ReimportRepairsConversationParticipant(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_REPAIR")
	require.NoError(os.MkdirAll(threadDir, 0o755))
	// The sender "Facebook User" is intentionally NOT in the participants
	// list, so the message goes through the synthesized-sender path. Only
	// the rehydration branch (not the thread-participants loop) would
	// re-link this participant on a later re-import.
	fixture := map[string]any{
		"participants": []map[string]any{
			{"name": "Test User"},
			{"name": "Alice Example"},
		},
		"messages": []map[string]any{
			{
				"sender_name":  "Facebook User",
				"timestamp_ms": 1600000000000,
				"content":      "system message",
				"type":         "Generic",
			},
		},
		"title":                "Alice Example",
		"is_still_participant": true,
		"thread_type":          "Regular",
		"thread_path":          "inbox/alice_REPAIR",
	}
	data, err := json.Marshal(fixture)
	require.NoError(err)
	writeFixture := func() {
		require.NoError(os.WriteFile(filepath.Join(threadDir, "message_1.json"), data, 0o644))
	}

	writeFixture()
	_, err = ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	var orphanID int64
	err = st.DB().QueryRow(
		`SELECT sender_id FROM messages WHERE source_message_id = 'inbox/alice_REPAIR__0'`,
	).Scan(&orphanID)
	require.NoError(err)

	// Simulate the pre-fix DB state: delete the synthesized sender's
	// conversation_participants row while leaving the message sender_id
	// intact. This is the scenario a database imported before synthesized
	// senders were linked would end up in.
	var convID int64
	err = st.DB().QueryRow(
		`SELECT id FROM conversations WHERE source_conversation_id = 'inbox/alice_REPAIR'`,
	).Scan(&convID)
	require.NoError(err)
	_, err = st.DB().Exec(
		st.Rebind(`DELETE FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`),
		convID, orphanID,
	)
	require.NoError(err)

	// Re-import with sender_name stripped so the current run can't
	// synthesize the orphan — rehydration is the only path that can
	// recover the participant link.
	fixture["messages"].([]map[string]any)[0]["sender_name"] = ""
	data, err = json.Marshal(fixture)
	require.NoError(err)
	writeFixture()
	_, err = ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)

	var n int
	err = st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`),
		convID, orphanID,
	).Scan(&n)
	require.NoError(err)
	assertpkg.Equal(t, 1, n, "conversation_participants not repaired on re-import")
}

func TestImportDYI_TimingTripwire(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeLargeFixture(t)
	start := time.Now()
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        root,
		AttachmentsDir: t.TempDir(),
	})
	requirepkg.NoError(t, err)
	elapsed := time.Since(start)
	assertpkg.Less(t, elapsed, 30*time.Second, "import took %v", elapsed)
	assertpkg.Equal(t, int64(largeFixtureSize), summary.MessagesAdded, "MessagesAdded")
}
