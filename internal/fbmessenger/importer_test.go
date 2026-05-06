package fbmessenger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
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
	if err != nil {
		t.Fatalf("ImportDYI(%s): %v", rootDir, err)
	}
	return summary
}

func countMessages(t *testing.T, st *store.Store, where string, args ...any) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM messages"
	if where != "" {
		q += " WHERE " + where
	}
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

func TestImportDYI_JSONSimple(t *testing.T) {
	st := testutil.NewTestStore(t)
	summary := importFixture(t, st, "testdata/json_simple")
	// json_simple has 1 inbox thread (3 messages) + 1 archived thread (1 message) = 4
	if summary.MessagesAdded != 4 {
		t.Errorf("MessagesAdded=%d want 4", summary.MessagesAdded)
	}
	if summary.HardErrors {
		t.Errorf("HardErrors=true")
	}
	if got := countMessages(t, st, "message_type='fbmessenger'"); got != 4 {
		t.Errorf("messages count=%d want 4", got)
	}
	if got := countMessages(t, st, "message_type='fbmessenger' AND sent_at IS NOT NULL"); got != 4 {
		t.Errorf("sent_at NULL rows exist: got %d want 4", got)
	}
	// Exactly one message_type present.
	rows, err := st.DB().Query("SELECT DISTINCT message_type FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var types []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		types = append(types, s)
	}
	if len(types) != 1 || types[0] != "fbmessenger" {
		t.Errorf("types=%v want [fbmessenger]", types)
	}
}

// TestImportDYI_MojibakeRepaired verifies mojibake repair on the body
// stored in message_bodies independently of FTS5. The FTS5 MATCH
// assertion lives in importer_fts_test.go gated on the fts5 build tag.
func TestImportDYI_MojibakeRepaired(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	var body string
	if err := st.DB().QueryRow(
		`SELECT body_text FROM message_bodies WHERE body_text LIKE '%café%'`,
	).Scan(&body); err != nil {
		t.Fatalf("body query: %v", err)
	}
	if !strings.Contains(body, "café") {
		t.Errorf("body=%q", body)
	}
}

func TestImportDYI_DirectChat(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	var ct string
	if err := st.DB().QueryRow(
		"SELECT conversation_type FROM conversations WHERE source_conversation_id='inbox/alice_ABC123'",
	).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if ct != "direct_chat" {
		t.Errorf("conv type=%q want direct_chat", ct)
	}
}

func TestImportDYI_GroupChat(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_group")
	var ct string
	if err := st.DB().QueryRow(
		"SELECT conversation_type FROM conversations WHERE source_conversation_id='inbox/crew_GRP123'",
	).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if ct != "group_chat" {
		t.Errorf("conv type=%q want group_chat", ct)
	}
	// Three facebook.messenger participants (Taylor/Alice/Bob) plus the
	// self seed. The self seed and the slug-derived sender address match
	// ("test.user@facebook.messenger"), so they collapse to one row.
	var n int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM participants WHERE domain='facebook.messenger'",
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Errorf("participants(fb)=%d want >=3", n)
	}
	// Every message has at least one 'to' recipient.
	var badMsgs int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.conversation_id = (SELECT id FROM conversations WHERE source_conversation_id='inbox/crew_GRP123')
		AND NOT EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id = m.id AND r.recipient_type='to')
	`).Scan(&badMsgs); err != nil {
		t.Fatal(err)
	}
	if badMsgs != 0 {
		t.Errorf("messages without 'to' recipients: %d", badMsgs)
	}
}

func TestImportDYI_MultifileNumericSort(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_multifile")
	rows, err := st.DB().Query(`
		SELECT source_message_id, sent_at FROM messages
		WHERE source_id = (SELECT id FROM sources WHERE source_type='facebook_messenger')
		ORDER BY sent_at ASC
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	var lastTime time.Time
	for rows.Next() {
		var id string
		var sentAt sql.NullTime
		if err := rows.Scan(&id, &sentAt); err != nil {
			t.Fatal(err)
		}
		if sentAt.Valid {
			if !sentAt.Time.After(lastTime) {
				t.Errorf("non-monotonic sent_at at %s", id)
			}
			lastTime = sentAt.Time
		}
		ids = append(ids, id)
	}
	if len(ids) != 4 {
		t.Fatalf("rows=%d want 4", len(ids))
	}
	// All source_message_id values must be prefixed dave_MULTI__ and
	// have monotonic index suffixes.
	for i, id := range ids {
		want := fmt.Sprintf("inbox/dave_MULTI__%d", i)
		if id != want {
			t.Errorf("source_message_id[%d]=%q want %q", i, id, want)
		}
	}
}

func TestImportDYI_Idempotent(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	before := snapshotRowCounts(t, st)
	_ = importFixture(t, st, "testdata/json_simple")
	after := snapshotRowCounts(t, st)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: before=%d after=%d", k, v, after[k])
		}
	}
}

func snapshotRowCounts(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	out := make(map[string]int)
	for _, tbl := range []string{"messages", "participants", "message_recipients", "attachments", "reactions", "conversations", "labels"} {
		var n int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

// A thread containing both a valid numbered file and an unrecognized
// sibling (e.g. message_final.json) must import the valid file and
// report the bad sibling via MessagesSkipped rather than aborting the
// entire thread.
func TestImportDYI_UnnumberedSiblingSkipped(t *testing.T) {
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "mixnames_OK")
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
	good := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","content":"good message"}
],"title":"mix"}`
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(good), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(threadPath, "message_final.json"), []byte(`not json`), 0644); err != nil {
		t.Fatal(err)
	}
	summary := importFixture(t, st, tmp)
	if summary.HardErrors {
		t.Errorf("HardErrors=true")
	}
	if summary.MessagesAdded != 1 {
		t.Errorf("MessagesAdded=%d want 1", summary.MessagesAdded)
	}
	if summary.FilesSkipped < 1 {
		t.Errorf("FilesSkipped=%d want >=1 (bad sibling)", summary.FilesSkipped)
	}
	if summary.MessagesSkipped != 0 {
		t.Errorf("MessagesSkipped=%d want 0 (no message was rejected)", summary.MessagesSkipped)
	}
	if summary.ThreadsSkipped != 0 {
		t.Errorf("ThreadsSkipped=%d want 0", summary.ThreadsSkipped)
	}
	// Valid file must be imported.
	var n int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/mixnames_OK'",
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("conversation not imported: n=%d", n)
	}
}

func TestImportDYI_CorruptSkipped(t *testing.T) {
	st := testutil.NewTestStore(t)
	summary := importFixture(t, st, "testdata/corrupt")
	if summary.HardErrors {
		t.Errorf("HardErrors=true")
	}
	if summary.ThreadsSkipped < 1 {
		t.Errorf("ThreadsSkipped=%d want >=1 (corrupt thread)", summary.ThreadsSkipped)
	}
	if summary.MessagesSkipped != 0 {
		t.Errorf("MessagesSkipped=%d want 0 (only whole-thread skip)", summary.MessagesSkipped)
	}
	// Good sibling message must still be imported.
	var n int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/goodsibling_OK'",
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("good sibling not imported: n=%d", n)
	}
}

func TestImportDYI_AttachmentStorage(t *testing.T) {
	st := testutil.NewTestStore(t)
	attachDir := t.TempDir()
	opts := ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        "testdata/json_with_media",
		Format:         "auto",
		AttachmentsDir: attachDir,
	}
	if _, err := ImportDYI(context.Background(), st, opts); err != nil {
		t.Fatal(err)
	}
	// Compute expected hash from fixture.
	png, err := os.ReadFile("testdata/json_with_media/your_activity_across_facebook/messages/inbox/bob_XYZ789/photos/tiny.png")
	if err != nil {
		t.Fatal(err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(png))

	var contentHash, storagePath string
	var size int64
	if err := st.DB().QueryRow(
		"SELECT content_hash, storage_path, size FROM attachments LIMIT 1",
	).Scan(&contentHash, &storagePath, &size); err != nil {
		t.Fatal(err)
	}
	if contentHash != wantHash {
		t.Errorf("content_hash=%q want %q", contentHash, wantHash)
	}
	if storagePath == "" {
		t.Error("storage_path empty")
	}
	absStorage := filepath.Join(attachDir, storagePath)
	got, err := os.ReadFile(absStorage)
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if string(got) != string(png) {
		t.Errorf("stored bytes differ")
	}
	if size != int64(len(png)) {
		t.Errorf("size=%d want %d", size, len(png))
	}
}

func TestImportDYI_AttachmentPathEscapeRejected(t *testing.T) {
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	// Build a fixture whose JSON references ../../etc/passwd.
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "evil_ESC")
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"../../etc/passwd"}]}
],"title":"x"}`
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.HardErrors {
		t.Errorf("HardErrors=true")
	}
	// Exactly one attachment row, with empty storage_path and content_hash.
	var sp, ch string
	if err := st.DB().QueryRow("SELECT storage_path, content_hash FROM attachments LIMIT 1").Scan(&sp, &ch); err != nil {
		t.Fatal(err)
	}
	if sp != "" || ch != "" {
		t.Errorf("path escape not rejected: storage_path=%q content_hash=%q", sp, ch)
	}
}

// TestImportDYI_AttachmentSymlinkRejected verifies that an attachment URI
// pointing at a symlink (e.g. a malicious DYI export that planted a
// symlink to a sensitive local file) is not followed: handleAttachment
// returns no storage_path/content_hash, so the symlink target is never
// copied into the attachment store.
func TestImportDYI_AttachmentSymlinkRejected(t *testing.T) {
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "evil_LNK")
	photosDir := filepath.Join(threadPath, "photos")
	if err := os.MkdirAll(photosDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create a "secret" file outside the attachment URI tree and a
	// symlink at the URI path that points at it. The URI itself stays
	// inside the export root, so the path-escape guard does not catch
	// it; only the symlink check does.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("password=hunter2"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(photosDir, "innocent.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"messages/inbox/evil_LNK/photos/innocent.png"}]}
],"title":"x"}`
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	attachmentsDir := t.TempDir()
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: attachmentsDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.HardErrors {
		t.Errorf("HardErrors=true")
	}
	var sp, ch string
	if err := st.DB().QueryRow("SELECT storage_path, content_hash FROM attachments LIMIT 1").Scan(&sp, &ch); err != nil {
		t.Fatal(err)
	}
	if sp != "" || ch != "" {
		t.Errorf("symlinked attachment not rejected: storage_path=%q content_hash=%q", sp, ch)
	}
	// Defense in depth: assert nothing under attachmentsDir contains the
	// secret bytes, so even a future copy regression would be caught.
	_ = filepath.Walk(attachmentsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "hunter2") {
			t.Errorf("symlink target leaked into attachments store at %s", p)
		}
		return nil
	})
}

func TestImportDYI_MissingAttachment(t *testing.T) {
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "missing_MIS")
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"messages/inbox/missing_MIS/photos/gone.png"}]}
],"title":"x"}`
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.HardErrors {
		t.Errorf("HardErrors=true")
	}
	var sp, ch string
	if err := st.DB().QueryRow("SELECT storage_path, content_hash FROM attachments LIMIT 1").Scan(&sp, &ch); err != nil {
		t.Fatal(err)
	}
	if sp != "" || ch != "" {
		t.Errorf("missing attachment should have empty storage_path: got sp=%q ch=%q", sp, ch)
	}
}

// TestImportDYI_ReactionsFirstClass verifies reaction rows and the
// "[reacted: ...]" body-append independently of FTS5. The FTS5 MATCH
// half of the dual-path lives in importer_fts_test.go.
func TestImportDYI_ReactionsFirstClass(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	var n int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM reactions r
		JOIN message_bodies b ON b.message_id = r.message_id
		WHERE b.body_text LIKE '%café%'
	`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("reactions=%d want 2", n)
	}
	var bodyCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_bodies WHERE body_text LIKE '%[reacted:%'`,
	).Scan(&bodyCount); err != nil {
		t.Fatal(err)
	}
	if bodyCount < 1 {
		t.Errorf("body with [reacted: suffix: got %d want >=1", bodyCount)
	}
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
		if err := st.DB().QueryRow(`
			SELECT b.body_text FROM message_bodies b
			JOIN messages m ON m.id = b.message_id
			WHERE m.source_message_id = ?`, id).Scan(&body); err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if body != wantBody {
			t.Errorf("%s: body=%q want %q", id, body, wantBody)
		}
	}
}

func TestImportDYI_MixedFormatJSONWins(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/mixed")
	// Exactly one conversation.
	var n int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/eve_MIX'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("conversations=%d want 1", n)
	}
	// 2 messages, no __html_ prefix.
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("messages=%d want 2", n)
	}
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_message_id LIKE '%html_%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("html_ prefixed rows=%d want 0", n)
	}
}

func TestImportDYI_FormatBoth(t *testing.T) {
	st := testutil.NewTestStore(t)
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        "testdata/mixed",
		Format:         "both",
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.HardErrors {
		t.Error("HardErrors=true")
	}
	var n int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("messages=%d want 4", n)
	}
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_message_id LIKE '%__html_%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("html rows=%d want 2", n)
	}
	// One conversation row, not two.
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM conversations WHERE source_conversation_id='inbox/eve_MIX'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("conversations=%d want 1", n)
	}
}

func TestImportDYI_IsFromMe(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        "testdata/json_simple",
		Format:         "auto",
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var ident string
	if err := st.DB().QueryRow(
		"SELECT identifier FROM sources WHERE source_type='facebook_messenger'",
	).Scan(&ident); err != nil {
		t.Fatal(err)
	}
	if ident != "test.user@facebook.messenger" {
		t.Errorf("identifier=%q", ident)
	}
	// Messages authored by Test User should have is_from_me=1.
	var wesFromMe, aliceFromMe int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.is_from_me = 1 AND m.source_message_id LIKE 'inbox/alice_ABC123__%'
	`).Scan(&wesFromMe); err != nil {
		t.Fatal(err)
	}
	if wesFromMe < 1 {
		t.Errorf("wes is_from_me rows=%d want >=1", wesFromMe)
	}
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM messages m
		WHERE m.is_from_me = 0 AND m.source_message_id LIKE 'inbox/alice_ABC123__%'
	`).Scan(&aliceFromMe); err != nil {
		t.Fatal(err)
	}
	if aliceFromMe < 1 {
		t.Errorf("alice is_from_me=0 rows=%d want >=1", aliceFromMe)
	}
}

func TestImportDYI_LabelTaxonomy(t *testing.T) {
	st := testutil.NewTestStore(t)
	_ = importFixture(t, st, "testdata/json_simple")
	// Messenger and Messenger / Inbox and Messenger / Archived must exist.
	for _, name := range []string{"Messenger", "Messenger / Inbox", "Messenger / Archived"} {
		var n int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM labels WHERE name = ?", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("label %q count=%d want 1", name, n)
		}
	}
	// Every inbox message has both Messenger and Messenger / Inbox labels.
	var n int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		JOIN messages m ON m.id = ml.message_id
		WHERE l.name = 'Messenger / Inbox'
		AND m.source_message_id LIKE 'inbox/alice_ABC123__%'
	`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("inbox labels on alice msgs: %d want 3", n)
	}
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		JOIN messages m ON m.id = ml.message_id
		WHERE l.name = 'Messenger / Archived'
		AND m.source_message_id LIKE 'archived_threads/zoe_ARCH__%'
	`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("archived labels on zoe msgs: %d want 1", n)
	}
}

func TestImportDYI_SelfParticipantSeeded(t *testing.T) {
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	// Empty DYI tree with just messages/inbox/.
	if err := os.MkdirAll(filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox"), 0755); err != nil {
		t.Fatal(err)
	}
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.MessagesProcessed != 0 {
		t.Errorf("MessagesProcessed=%d want 0", summary.MessagesProcessed)
	}
	if summary.HardErrors {
		t.Error("HardErrors=true")
	}
	var n int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM participants WHERE email_address = ? AND domain = 'facebook.messenger'",
		"test.user@facebook.messenger",
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("self participant count=%d want 1", n)
	}
}

func TestImportDYI_MeDomainValidation(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "wes@gmail.com",
		RootDir:        "testdata/json_simple",
		AttachmentsDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "facebook.messenger") {
		t.Errorf("error should mention facebook.messenger, got %v", err)
	}
	var n int
	if err := st.DB().QueryRow(
		"SELECT COUNT(*) FROM sources WHERE source_type='facebook_messenger'",
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sources=%d want 0", n)
	}
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
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
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
	for i := 0; i < largeFixtureSize; i++ {
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
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return tmp
}

// writeMultiThreadFixture lays out `n` inbox threads under a DYI root
// at tmp, each with a single short message. Threads are named
// "thread_{i}_OK" so that Discover sorts them deterministically.
func writeMultiThreadFixture(t *testing.T, n int) string {
	t.Helper()
	tmp := t.TempDir()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("thread_%02d_OK", i)
		threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", name)
		if err := os.MkdirAll(threadPath, 0755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(
			`{"participants":[{"name":"Test User"},{"name":"Friend %d"}],"messages":[`+
				`{"sender_name":"Friend %d","timestamp_ms":%d,"type":"Generic","content":"hello from %d"}`+
				`],"title":"Friend %d"}`,
			i, i, 1600000000000+int64(i)*60000, i, i,
		)
		if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return tmp
}

// TestImportDYI_ResumeFromCheckpoint seeds an active sync with a prior
// fbmessengerCheckpoint pointing past the first thread, then runs
// ImportDYI and verifies that (a) WasResumed is true and (b) the
// already-processed thread is skipped on the second run (while still
// present in the store from the first run so idempotence holds).
func TestImportDYI_ResumeFromCheckpoint(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 3)

	// First run: import everything. This populates the DB and leaves
	// no checkpoint (CompleteSync clears cursor_before in practice
	// because we only read when there's an *active* run; a completed
	// run is fine to coexist).
	first := importFixture(t, st, root)
	if first.WasResumed {
		t.Errorf("first run WasResumed=true, want false")
	}
	if first.MessagesAdded != 3 {
		t.Fatalf("first run MessagesAdded=%d want 3", first.MessagesAdded)
	}
	if first.ThreadsProcessed != 3 {
		t.Fatalf("first run ThreadsProcessed=%d want 3", first.ThreadsProcessed)
	}

	// Simulate an in-progress run: create a new running sync_run for
	// the facebook_messenger source and write a fbmessengerCheckpoint
	// whose ThreadIndex == 2 (two threads already done).
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	if err != nil {
		t.Fatal(err)
	}
	syncID, err := st.StartSync(src.ID, "import-messenger")
	if err != nil {
		t.Fatal(err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:          absRoot,
		ThreadIndex:      2,
		LastMessageIndex: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         string(cpJSON),
		MessagesProcessed: 2,
		MessagesAdded:     2,
	}); err != nil {
		t.Fatal(err)
	}

	// Second run: should detect the active checkpoint and resume,
	// processing only the 3rd thread.
	before := snapshotRowCounts(t, st)
	second := importFixture(t, st, root)
	after := snapshotRowCounts(t, st)

	if !second.WasResumed {
		t.Errorf("second run WasResumed=false, want true")
	}
	if second.ThreadsProcessed != 1 {
		t.Errorf("second run ThreadsProcessed=%d want 1 (only last thread)", second.ThreadsProcessed)
	}
	// Idempotence: row counts must not change (source_message_id
	// dedupes the one re-imported thread if it were processed; but
	// here the resume skip means it is not touched at all).
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: before=%d after=%d", k, v, after[k])
		}
	}
	// All three threads must still be present.
	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE source_conversation_id LIKE 'inbox/thread_%_OK'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("conversations=%d want 3", n)
	}
}

// TestImportDYI_ResumeWrongRootRejected verifies that a prior
// checkpoint for a different RootDir is rejected.
func TestImportDYI_ResumeWrongRootRejected(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 2)

	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	if err != nil {
		t.Fatal(err)
	}
	syncID, err := st.StartSync(src.ID, "import-messenger")
	if err != nil {
		t.Fatal(err)
	}
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:     "/some/other/dir",
		ThreadIndex: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken: string(cpJSON),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        root,
		AttachmentsDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for wrong root, got nil")
	}
	if !strings.Contains(err.Error(), "different root") {
		t.Errorf("error=%v want mention of 'different root'", err)
	}
}

// TestImportDYI_ResumeFromFailedSync verifies that a checkpoint saved
// before FailSync is still found on the next run, so interrupted imports
// can resume instead of restarting from scratch.
func TestImportDYI_ResumeFromFailedSync(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 3)

	// First run: import everything.
	first := importFixture(t, st, root)
	if first.MessagesAdded != 3 {
		t.Fatalf("first run MessagesAdded=%d want 3", first.MessagesAdded)
	}

	// Simulate a failed (interrupted) sync: create a sync run, save a
	// checkpoint, then mark it failed — mimicking what happens when the
	// user hits Ctrl-C.
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	if err != nil {
		t.Fatal(err)
	}
	syncID, err := st.StartSync(src.ID, "import-messenger")
	if err != nil {
		t.Fatal(err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:     absRoot,
		ThreadIndex: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         string(cpJSON),
		MessagesProcessed: 2,
		MessagesAdded:     2,
	}); err != nil {
		t.Fatal(err)
	}
	// Mark the sync as failed, simulating a graceful interrupt.
	if err := st.FailSync(syncID, "context canceled"); err != nil {
		t.Fatal(err)
	}

	// The next run must find the failed sync's checkpoint and resume.
	second := importFixture(t, st, root)
	if !second.WasResumed {
		t.Errorf("second run WasResumed=false, want true")
	}
	if second.ThreadsProcessed != 1 {
		t.Errorf("second run ThreadsProcessed=%d want 1 (only last thread)", second.ThreadsProcessed)
	}
}

// TestImportDYI_ResumeFromFirstThreadCheckpoint verifies that a
// checkpoint saved while still processing the first thread
// (ThreadIndex == 0) is treated as resumable rather than ignored.
// source_message_id dedup covers the data path; this test asserts the
// UX-visible state (WasResumed true and cumulative counters carried
// forward) so a user-visible interrupt during thread 0 is reflected in
// the next run's summary.
func TestImportDYI_ResumeFromFirstThreadCheckpoint(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 2)

	// Seed a failed sync whose checkpoint is mid-first-thread.
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	if err != nil {
		t.Fatal(err)
	}
	syncID, err := st.StartSync(src.ID, "import-messenger")
	if err != nil {
		t.Fatal(err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{
		RootDir:          absRoot,
		ThreadIndex:      0,
		LastMessageIndex: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken:         string(cpJSON),
		MessagesProcessed: 1,
		MessagesAdded:     1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FailSync(syncID, "context canceled"); err != nil {
		t.Fatal(err)
	}

	summary := importFixture(t, st, root)
	if !summary.WasResumed {
		t.Errorf("WasResumed=false, want true for first-thread checkpoint")
	}
	// Cumulative counters must carry over from the prior run.
	if summary.MessagesProcessed < 1 {
		t.Errorf("MessagesProcessed=%d, want carry-over from prior run (>=1)", summary.MessagesProcessed)
	}
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
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
	if !strings.Contains(err.Error(), "unknown --format") {
		t.Errorf("error=%v want mention of 'unknown --format'", err)
	}
}

// TestImportDYI_StaleFailedCheckpointIgnoredAfterCompletion verifies that a
// failed run's checkpoint is not used to resume a later import once a
// successful run has occurred since. Without the fix,
// GetLatestCheckpointedSync would still return the older failed run because
// its status filter (running/failed) excluded the more recent completed
// run, and a re-import would silently resume from the stale checkpoint
// and skip threads already covered by the successful run.
func TestImportDYI_StaleFailedCheckpointIgnoredAfterCompletion(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 3)

	// Seed a failed sync with a checkpoint pointing past thread 0.
	src, err := st.GetOrCreateSource("facebook_messenger", "test.user@facebook.messenger")
	if err != nil {
		t.Fatal(err)
	}
	failID, err := st.StartSync(src.ID, "import-messenger")
	if err != nil {
		t.Fatal(err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	cpJSON, err := json.Marshal(fbmessengerCheckpoint{RootDir: absRoot, ThreadIndex: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSyncCheckpoint(failID, &store.Checkpoint{
		PageToken: string(cpJSON), MessagesProcessed: 2, MessagesAdded: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FailSync(failID, "context canceled"); err != nil {
		t.Fatal(err)
	}

	// Run a successful import after the failed run. This becomes the
	// latest sync, so a future re-import must NOT resume from the older
	// failed checkpoint.
	first := importFixture(t, st, root)
	if first.MessagesAdded != 3 {
		t.Fatalf("first run MessagesAdded=%d want 3", first.MessagesAdded)
	}

	second := importFixture(t, st, root)
	if second.WasResumed {
		t.Errorf("WasResumed=true: stale failed checkpoint resumed despite later completed run")
	}
	if second.ThreadsProcessed != 3 {
		t.Errorf("ThreadsProcessed=%d want 3 (full re-scan)", second.ThreadsProcessed)
	}
}

// TestImportDYI_ReimportPicksUpNewMessages verifies that re-importing a root
// after a successful import picks up new messages added to an existing thread,
// rather than treating the completed run as resumable and skipping threads.
// Regression test for: GetLatestCheckpointedSync matching completed runs.
func TestImportDYI_ReimportPicksUpNewMessages(t *testing.T) {
	// Copy json_simple fixture to a temp dir so we can mutate it.
	root := t.TempDir()
	cpDir(t, "testdata/json_simple", root)

	st := testutil.NewTestStore(t)

	// First import: 4 messages (3 inbox + 1 archived).
	s1 := importFixture(t, st, root)
	if s1.MessagesAdded != 4 {
		t.Fatalf("first import: MessagesAdded=%d want 4", s1.MessagesAdded)
	}
	before := countMessages(t, st, "message_type='fbmessenger'")
	if before != 4 {
		t.Fatalf("messages after first import=%d want 4", before)
	}

	// Add a new message to the existing alice thread.
	threadFile := filepath.Join(root, "your_activity_across_facebook/messages/inbox/alice_ABC123/message_1.json")
	raw, err := os.ReadFile(threadFile)
	if err != nil {
		t.Fatal(err)
	}
	var thread map[string]any
	if err := json.Unmarshal(raw, &thread); err != nil {
		t.Fatal(err)
	}
	msgs := thread["messages"].([]any)
	newMsg := map[string]any{
		"sender_name":  "Alice Example",
		"timestamp_ms": float64(1600000200000),
		"content":      "New message after first import",
		"type":         "Generic",
	}
	thread["messages"] = append([]any{newMsg}, msgs...)
	updated, err := json.MarshalIndent(thread, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(threadFile, updated, 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-import the same root. The new message must be picked up.
	s2 := importFixture(t, st, root)
	after := countMessages(t, st, "message_type='fbmessenger'")
	if after != before+1 {
		t.Errorf("messages after re-import=%d want %d (added=%d)", after, before+1, s2.MessagesAdded)
	}
}

// cpDir recursively copies src into dst.
func cpDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dp, 0o755); err != nil {
				t.Fatal(err)
			}
			cpDir(t, sp, dp)
		} else {
			data, err := os.ReadFile(sp)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dp, data, 0o644); err != nil {
				t.Fatal(err)
			}
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
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_ORPH")
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(threadDir, "message_1.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The synthesized "Facebook User" sender must be linked to the
	// conversation via conversation_participants, not just present as
	// sender_id on its message.
	var n int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = (
			SELECT id FROM conversations WHERE source_conversation_id = 'inbox/alice_ORPH'
		)
		AND p.email_address = 'facebook.user@facebook.messenger'
	`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("orphan sender not linked to conversation: got %d want 1", n)
	}
}

// TestImportDYI_SenderIDPreservedOnReimport verifies that re-importing a
// thread whose message sender no longer resolves (e.g. the participant
// was renamed or the second import uses a different fixture) does not
// null-out the previously-recorded messages.sender_id, display name, or
// is_from_me flag. The importer reads any existing sender data and reuses
// it when the current run can't produce one.
func TestImportDYI_SenderIDPreservedOnReimport(t *testing.T) {
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_PRES")
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatal(err)
	}

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
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(threadDir, "message_1.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// First import: message 0 from Alice, message 1 from Test User (self).
	write("Alice Example", "Test User")
	if _, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}

	type snap struct {
		senderID sql.NullInt64
		isFromMe bool
		fromName sql.NullString
		fromPID  sql.NullInt64
	}
	capture := func(srcMsgID string) snap {
		t.Helper()
		var s snap
		if err := st.DB().QueryRow(
			`SELECT sender_id, is_from_me FROM messages WHERE source_message_id = ?`,
			srcMsgID,
		).Scan(&s.senderID, &s.isFromMe); err != nil {
			t.Fatalf("messages row for %s: %v", srcMsgID, err)
		}
		if err := st.DB().QueryRow(`
			SELECT mr.display_name, mr.participant_id
			FROM message_recipients mr
			JOIN messages m ON m.id = mr.message_id
			WHERE m.source_message_id = ? AND mr.recipient_type = 'from'
		`, srcMsgID).Scan(&s.fromName, &s.fromPID); err != nil {
			t.Fatalf("from recipient for %s: %v", srcMsgID, err)
		}
		return s
	}

	aliceBefore := capture("inbox/alice_PRES__0")
	selfBefore := capture("inbox/alice_PRES__1")
	if !aliceBefore.senderID.Valid || aliceBefore.isFromMe {
		t.Fatalf("alice msg setup: senderID=%v isFromMe=%v", aliceBefore.senderID, aliceBefore.isFromMe)
	}
	if !selfBefore.senderID.Valid || !selfBefore.isFromMe {
		t.Fatalf("self msg setup: senderID=%v isFromMe=%v", selfBefore.senderID, selfBefore.isFromMe)
	}

	// Second import: both sender_names stripped so the current run can't
	// resolve them. The importer must preserve prior sender_id, is_from_me,
	// and message_recipients for both messages.
	write("", "")
	summary, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Rehydrated self-authored messages must still count toward
	// FromMeCount so the CLI doesn't warn about a --me mismatch.
	if summary.FromMeCount < 1 {
		t.Errorf("FromMeCount=%d want >=1 on rehydration", summary.FromMeCount)
	}

	aliceAfter := capture("inbox/alice_PRES__0")
	selfAfter := capture("inbox/alice_PRES__1")

	if !aliceAfter.senderID.Valid || aliceAfter.senderID.Int64 != aliceBefore.senderID.Int64 {
		t.Errorf("alice sender_id not preserved: before=%v after=%v", aliceBefore.senderID, aliceAfter.senderID)
	}
	if aliceAfter.isFromMe {
		t.Errorf("alice is_from_me flipped to true")
	}
	if !aliceAfter.fromName.Valid || aliceAfter.fromName.String != "Alice Example" {
		t.Errorf("alice from display_name=%q want Alice Example", aliceAfter.fromName.String)
	}
	if !aliceAfter.fromPID.Valid || aliceAfter.fromPID.Int64 != aliceBefore.senderID.Int64 {
		t.Errorf("alice from participant_id not preserved: got %v want %d", aliceAfter.fromPID, aliceBefore.senderID.Int64)
	}

	if !selfAfter.senderID.Valid || selfAfter.senderID.Int64 != selfBefore.senderID.Int64 {
		t.Errorf("self sender_id not preserved: before=%v after=%v", selfBefore.senderID, selfAfter.senderID)
	}
	if !selfAfter.isFromMe {
		t.Errorf("self is_from_me not preserved (flipped to false)")
	}
	// The self participant is seeded with an empty participants.display_name,
	// so rehydration must fall back to the prior message_recipients display
	// name rather than clobbering it with "".
	if !selfAfter.fromName.Valid || selfAfter.fromName.String != "Test User" {
		t.Errorf("self from display_name=%q want Test User", selfAfter.fromName.String)
	}

	// The account owner must NOT appear in "to" for the self-authored
	// message — otherwise the dropped is_from_me flag would inflate
	// participant analytics and cause self-to-self recipient rows.
	var selfInTo int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = 'inbox/alice_PRES__1'
		  AND mr.recipient_type = 'to'
		  AND mr.participant_id = ?
	`, selfBefore.senderID.Int64).Scan(&selfInTo); err != nil {
		t.Fatal(err)
	}
	if selfInTo != 0 {
		t.Errorf("self participant appeared in 'to' for self-authored message: count=%d", selfInTo)
	}
}

// TestImportDYI_ReimportRepairsConversationParticipant verifies that a
// conversation missing a participant row (the pre-fix state for databases
// imported before synthesized senders were linked) gets re-linked on a
// subsequent import via the sender_id-preservation rehydration path.
func TestImportDYI_ReimportRepairsConversationParticipant(t *testing.T) {
	st := testutil.NewTestStore(t)
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_REPAIR")
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	writeFixture := func() {
		if err := os.WriteFile(filepath.Join(threadDir, "message_1.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFixture()
	if _, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	var orphanID int64
	if err := st.DB().QueryRow(
		`SELECT sender_id FROM messages WHERE source_message_id = 'inbox/alice_REPAIR__0'`,
	).Scan(&orphanID); err != nil {
		t.Fatal(err)
	}

	// Simulate the pre-fix DB state: delete the synthesized sender's
	// conversation_participants row while leaving the message sender_id
	// intact. This is the scenario a database imported before synthesized
	// senders were linked would end up in.
	var convID int64
	if err := st.DB().QueryRow(
		`SELECT id FROM conversations WHERE source_conversation_id = 'inbox/alice_REPAIR'`,
	).Scan(&convID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`DELETE FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`,
		convID, orphanID,
	); err != nil {
		t.Fatal(err)
	}

	// Re-import with sender_name stripped so the current run can't
	// synthesize the orphan — rehydration is the only path that can
	// recover the participant link.
	fixture["messages"].([]map[string]any)[0]["sender_name"] = ""
	data, err = json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture()
	if _, err := ImportDYI(context.Background(), st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        tmp,
		AttachmentsDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`,
		convID, orphanID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("conversation_participants not repaired on re-import: got %d want 1", n)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed > 30*time.Second {
		t.Errorf("import took %v, want < 30s", elapsed)
	}
	if summary.MessagesAdded != int64(largeFixtureSize) {
		t.Errorf("MessagesAdded=%d want %d", summary.MessagesAdded, largeFixtureSize)
	}
}
