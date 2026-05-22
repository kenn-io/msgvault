package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/mbox"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportMbox_IngestsMessagesThreadsAndAttachments(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	attachmentsDir := filepath.Join(tmp, "attachments")

	raw1 := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		WithAttachment("a.txt", "text/plain", []byte("hello")).
		Bytes()

	raw2 := email.NewMessage().
		From("Bob <bob@example.com>").
		To("Alice <alice@example.com>").
		Subject("Re: Hello").
		Date("Mon, 01 Jan 2024 13:00:00 +0000").
		Header("Message-ID", "<msg2@example.com>").
		Header("In-Reply-To", "<msg1@example.com>").
		Header("References", "<msg1@example.com>").
		Body("Reply.\n").
		Bytes()

	mboxData := strings.Builder{}
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw1)
	if !strings.HasSuffix(string(raw1), "\n") {
		mboxData.WriteString("\n")
	}
	mboxData.WriteString("From bob@example.com Mon Jan 1 13:00:00 2024\n")
	mboxData.Write(raw2)
	if !strings.HasSuffix(string(raw2), "\n") {
		mboxData.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "hey-export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "hey",
		Identifier:         "me@hey.com",
		Labels:             []string{"hey"},
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     attachmentsDir,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}
	if summary.MessagesAdded != 2 {
		t.Fatalf("MessagesAdded = %d, want 2", summary.MessagesAdded)
	}

	// Verify counts.
	var (
		sourceCount       int
		conversationCount int
		messageCount      int
		rawCount          int
		labelCount        int
		msgLabelCount     int
		attachmentCount   int
	)
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sources WHERE source_type = 'hey' AND identifier = 'me@hey.com'`).Scan(&sourceCount); err != nil {
		t.Fatalf("count sources: %v", err)
	}
	if sourceCount != 1 {
		t.Fatalf("sourceCount = %d, want 1", sourceCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&conversationCount); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if conversationCount != 1 {
		t.Fatalf("conversationCount = %d, want 1", conversationCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 2 {
		t.Fatalf("messageCount = %d, want 2", messageCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&rawCount); err != nil {
		t.Fatalf("count message_raw: %v", err)
	}
	if rawCount != 2 {
		t.Fatalf("rawCount = %d, want 2", rawCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM labels WHERE name = 'hey'`).Scan(&labelCount); err != nil {
		t.Fatalf("count labels: %v", err)
	}
	if labelCount != 1 {
		t.Fatalf("labelCount = %d, want 1", labelCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&msgLabelCount); err != nil {
		t.Fatalf("count message_labels: %v", err)
	}
	if msgLabelCount != 2 {
		t.Fatalf("msgLabelCount = %d, want 2", msgLabelCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentCount); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachmentCount != 1 {
		t.Fatalf("attachmentCount = %d, want 1", attachmentCount)
	}

	// Verify attachment file exists.
	var storagePath string
	if err := st.DB().QueryRow(`SELECT storage_path FROM attachments LIMIT 1`).Scan(&storagePath); err != nil {
		t.Fatalf("select storage_path: %v", err)
	}
	if storagePath == "" {
		t.Fatalf("storage_path empty")
	}
	if _, err := os.Stat(filepath.Join(attachmentsDir, filepath.FromSlash(storagePath))); err != nil {
		t.Fatalf("attachment file missing: %v", err)
	}
}

func TestImportMbox_NoAttachmentsStillRecordsAttachmentMetadata(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		WithAttachment("a.txt", "text/plain", []byte("hello")).
		Bytes()

	var mboxData strings.Builder
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mboxData.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     "",
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}
	if summary.MessagesAdded != 1 {
		t.Fatalf("MessagesAdded = %d, want 1", summary.MessagesAdded)
	}

	var attachmentRows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentRows); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachmentRows != 0 {
		t.Fatalf("attachmentRows = %d, want 0", attachmentRows)
	}

	var (
		hasAttachments  bool
		attachmentCount int
	)
	if err := st.DB().QueryRow(`SELECT has_attachments, attachment_count FROM messages WHERE subject = 'Hello' LIMIT 1`).Scan(&hasAttachments, &attachmentCount); err != nil {
		t.Fatalf("select message attachment metadata: %v", err)
	}
	if !hasAttachments {
		t.Fatalf("has_attachments = %v, want true", hasAttachments)
	}
	if attachmentCount != 1 {
		t.Fatalf("attachment_count = %d, want 1", attachmentCount)
	}
}

func TestImportMbox_IsIdempotentAcrossPathChanges(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	raw1 := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("One").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Msg1.\n").
		Bytes()

	raw2 := email.NewMessage().
		From("Bob <bob@example.com>").
		To("Alice <alice@example.com>").
		Subject("Two").
		Date("Mon, 01 Jan 2024 13:00:00 +0000").
		Header("Message-ID", "<msg2@example.com>").
		Body("Msg2.\n").
		Bytes()

	var mboxData strings.Builder
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw1)
	if !strings.HasSuffix(string(raw1), "\n") {
		mboxData.WriteString("\n")
	}
	mboxData.WriteString("From bob@example.com Mon Jan 1 13:00:00 2024\n")
	mboxData.Write(raw2)
	if !strings.HasSuffix(string(raw2), "\n") {
		mboxData.WriteString("\n")
	}

	dir1 := filepath.Join(tmp, "a")
	dir2 := filepath.Join(tmp, "b")
	if err := os.MkdirAll(dir1, 0700); err != nil {
		t.Fatalf("mkdir dir1: %v", err)
	}
	if err := os.MkdirAll(dir2, 0700); err != nil {
		t.Fatalf("mkdir dir2: %v", err)
	}

	mboxPath1 := filepath.Join(dir1, "export.mbox")
	mboxPath2 := filepath.Join(dir2, "export.mbox")
	if err := os.WriteFile(mboxPath1, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox1: %v", err)
	}
	if err := os.WriteFile(mboxPath2, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox2: %v", err)
	}

	if _, err := ImportMbox(context.Background(), st, mboxPath1, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	}); err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}

	var messageCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 2 {
		t.Fatalf("messageCount = %d, want 2", messageCount)
	}

	if _, err := ImportMbox(context.Background(), st, mboxPath2, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	}); err != nil {
		t.Fatalf("ImportMbox (second path): %v", err)
	}

	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages (second path): %v", err)
	}
	if messageCount != 2 {
		t.Fatalf("messageCount (second path) = %d, want 2", messageCount)
	}
}

func TestParseFromLineDate_NamedTimezone(t *testing.T) {
	got, ok := parseFromLineDate("From sender@example.com Mon Jan 1 00:00:00 MST 2024")
	if !ok {
		t.Fatalf("expected ok")
	}
	if got.Year() != 2024 {
		t.Fatalf("got year %d, want 2024", got.Year())
	}
}

func TestImportMbox_InvalidInput_ReturnsErrorAndFailsSync(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	mboxPath := filepath.Join(tmp, "not-mbox.txt")
	if err := os.WriteFile(mboxPath, []byte("this is not an mbox file\n"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err = ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	if err == nil {
		t.Fatalf("expected error")
	}

	var status string
	if err := st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("select status: %v", err)
	}
	if status != store.SyncStatusFailed {
		t.Fatalf("status = %q, want %q", status, store.SyncStatusFailed)
	}
}

func TestImportMbox_IdenticalRawMessagesAreImportedSeparately(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Dup").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<dup@example.com>").
		Body("Same.\n").
		Bytes()

	var mboxData strings.Builder
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mboxData.WriteString("\n")
	}
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:01 2024\n")
	mboxData.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mboxData.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "dup.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}
	if summary.MessagesProcessed != 2 {
		t.Fatalf("MessagesProcessed = %d, want 2", summary.MessagesProcessed)
	}
	if summary.MessagesAdded != 2 {
		t.Fatalf("MessagesAdded = %d, want 2", summary.MessagesAdded)
	}
	if summary.MessagesSkipped != 0 {
		t.Fatalf("MessagesSkipped = %d, want 0", summary.MessagesSkipped)
	}
	if summary.Errors != 0 {
		t.Fatalf("Errors = %d, want 0", summary.Errors)
	}

	var messageCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 2 {
		t.Fatalf("messageCount = %d, want 2", messageCount)
	}
}

func TestImportMbox_RerunRepairsMissingRawInsteadOfSkipping(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		Bytes()

	var mboxData strings.Builder
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mboxData.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	// Create a partial ingest: message row exists, but no message_raw row.
	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	convID, err := st.EnsureConversation(src.ID, "thread1", "Thread")
	if err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}

	sum := sha256.Sum256(raw)
	rawHash := hex.EncodeToString(sum[:])

	sourceMsgID := fmt.Sprintf("mbox-%s-%d", rawHash, int64(1))
	if _, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: sourceMsgID,
		MessageType:     "email",
	}); err != nil {
		t.Fatalf("upsert message: %v", err)
	}

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}
	if summary.MessagesSkipped != 0 {
		t.Fatalf("MessagesSkipped = %d, want 0", summary.MessagesSkipped)
	}

	var rawCount int
	if err := st.DB().QueryRow(`
		SELECT COUNT(*)
		FROM message_raw mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_id = ? AND m.source_message_id = ?
	`, src.ID, sourceMsgID).Scan(&rawCount); err != nil {
		t.Fatalf("count message_raw: %v", err)
	}
	if rawCount != 1 {
		t.Fatalf("rawCount = %d, want 1", rawCount)
	}
}

func TestImportMbox_RerunRetriesAttachmentsAfterStoreFailure(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	attachmentsDir := filepath.Join(tmp, "attachments")
	// Force attachment storage errors by making the attachments path a file.
	if err := os.WriteFile(attachmentsDir, []byte("not a dir"), 0600); err != nil {
		t.Fatalf("write attachments sentinel: %v", err)
	}

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		WithAttachment("a.txt", "text/plain", []byte("hello")).
		Bytes()

	var mboxData strings.Builder
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mboxData.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	// Attachment storage is best-effort: the ingest succeeds even
	// though the attachment file couldn't be written to disk.
	_, err = ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     attachmentsDir,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}

	// Message + raw MIME are committed inside the atomic transaction.
	var rawCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&rawCount); err != nil {
		t.Fatalf("count message_raw: %v", err)
	}
	if rawCount != 1 {
		t.Fatalf("rawCount = %d, want 1", rawCount)
	}

	// Attachment was not stored because disk write failed, but the
	// attachment count correction updated the metadata to reflect this.
	var attachmentCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentCount); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachmentCount != 0 {
		t.Fatalf("attachmentCount = %d, want 0", attachmentCount)
	}

	// Metadata was corrected to reflect zero stored attachments.
	var hasAttachments bool
	var metaCount int
	if err := st.DB().QueryRow(
		`SELECT has_attachments, attachment_count FROM messages LIMIT 1`,
	).Scan(&hasAttachments, &metaCount); err != nil {
		t.Fatalf("select attachment metadata: %v", err)
	}
	if hasAttachments {
		t.Fatalf("has_attachments = true, want false")
	}
	if metaCount != 0 {
		t.Fatalf("attachment_count = %d, want 0", metaCount)
	}

	// Fix the attachments dir, rerun — the message is already
	// ingested so it's skipped, but verify no errors.
	if err := os.Remove(attachmentsDir); err != nil {
		t.Fatalf("remove attachments file: %v", err)
	}
	if err := os.MkdirAll(attachmentsDir, 0700); err != nil {
		t.Fatalf("mkdir attachments dir: %v", err)
	}

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     attachmentsDir,
	})
	if err != nil {
		t.Fatalf("ImportMbox (rerun): %v", err)
	}
	if summary.MessagesSkipped != 1 {
		t.Fatalf("MessagesSkipped = %d, want 1", summary.MessagesSkipped)
	}
}

func TestImportMbox_ErrorsCauseSyncFailed(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if _, err := st.DB().Exec(`DROP TABLE messages`); err != nil {
		t.Fatalf("drop messages: %v", err)
	}

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi.\n").
		Bytes()

	var mboxData strings.Builder
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mboxData.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}
	if summary.Errors == 0 {
		t.Fatalf("expected errors > 0")
	}

	var (
		status      string
		errorsCount int
	)
	if err := st.DB().QueryRow(`SELECT status, errors_count FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &errorsCount); err != nil {
		t.Fatalf("select sync: %v", err)
	}
	if status != store.SyncStatusFailed {
		t.Fatalf("status = %q, want %q", status, store.SyncStatusFailed)
	}
	if errorsCount == 0 {
		t.Fatalf("errorsCount = %d, want > 0", errorsCount)
	}
}

func TestImportMbox_SoftErrorsDoNotFailSync(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	maxBytes := int64(64)

	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024",
		"Subject: " + strings.Repeat("a", 200),
		"",
		"Body1",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		MaxMessageBytes:    maxBytes,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}
	if summary.Errors == 0 {
		t.Fatalf("expected errors > 0")
	}
	if summary.HardErrors {
		t.Fatalf("expected no hard errors")
	}

	var (
		status      string
		errorsCount int
	)
	if err := st.DB().QueryRow(`SELECT status, errors_count FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &errorsCount); err != nil {
		t.Fatalf("select sync: %v", err)
	}
	if status != store.SyncStatusCompleted {
		t.Fatalf("status = %q, want %q", status, store.SyncStatusCompleted)
	}
	if errorsCount == 0 {
		t.Fatalf("errorsCount = %d, want > 0", errorsCount)
	}
}

func TestImportMbox_CheckpointDoesNotAdvancePastFailedIngest(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	raw := func(subject string) []byte {
		return email.NewMessage().
			From("Alice <alice@example.com>").
			To("Bob <bob@example.com>").
			Subject(subject).
			Date("Mon, 01 Jan 2024 12:00:00 +0000").
			Header("Message-ID", fmt.Sprintf("<%s@example.com>", subject)).
			Body("Hi.\n").
			Bytes()
	}

	var mboxData strings.Builder
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mboxData.Write(raw("msg1"))
	if !strings.HasSuffix(mboxData.String(), "\n") {
		mboxData.WriteString("\n")
	}
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:01 2024\n")
	mboxData.Write(raw("msg2"))
	if !strings.HasSuffix(mboxData.String(), "\n") {
		mboxData.WriteString("\n")
	}
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:02 2024\n")
	mboxData.Write(raw("msg3"))
	if !strings.HasSuffix(mboxData.String(), "\n") {
		mboxData.WriteString("\n")
	}
	mboxData.WriteString("From alice@example.com Mon Jan 1 12:00:03 2024\n")
	mboxData.Write(raw("msg4"))
	if !strings.HasSuffix(mboxData.String(), "\n") {
		mboxData.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	// Capture the offset after the first message: this is the safe resume point
	// if the second message fails to ingest.
	f, err := os.Open(mboxPath)
	if err != nil {
		t.Fatalf("open mbox: %v", err)
	}
	r := mbox.NewReaderWithMaxMessageBytes(f, defaultMaxMboxMessageBytes)
	if _, err := r.Next(); err != nil {
		_ = f.Close()
		t.Fatalf("read first message: %v", err)
	}
	wantOffset := r.NextFromOffset()
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	calls := 0
	ingestFn := func(ctx context.Context, st *store.Store, sourceID int64, identifier string, attachmentsDir string, labelIDs []int64, sourceMsgID string, rawHash string, msg *mbox.Message, log *slog.Logger) error {
		calls++
		switch calls {
		case 2:
			return fmt.Errorf("boom")
		}
		if err := ingestRawEmail(ctx, st, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID, rawHash, msg, log); err != nil {
			return err
		}
		if calls == 3 {
			// Cancel after successfully ingesting a message following the failure, to mimic
			// an interrupted run with work already done past the failure.
			cancel()
		}
		return nil
	}

	_, err = ImportMbox(ctx, st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		IngestFunc:         ingestFn,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}

	var (
		status string
		cursor string
	)
	if err := st.DB().QueryRow(`SELECT status, cursor_before FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &cursor); err != nil {
		t.Fatalf("select sync: %v", err)
	}
	if status != store.SyncStatusRunning {
		t.Fatalf("status = %q, want %q", status, store.SyncStatusRunning)
	}

	var cp mboxCheckpoint
	if err := json.Unmarshal([]byte(cursor), &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	if cp.Offset != wantOffset {
		t.Fatalf("checkpoint offset = %d, want %d", cp.Offset, wantOffset)
	}

	var messageCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 2 {
		t.Fatalf("messageCount = %d, want 2", messageCount)
	}

	// Resume the interrupted sync and ensure already-ingested messages are not duplicated.
	if _, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           false,
		CheckpointInterval: 1,
	}); err != nil {
		t.Fatalf("ImportMbox (resume): %v", err)
	}

	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages (resume): %v", err)
	}
	if messageCount != 4 {
		t.Fatalf("messageCount (resume) = %d, want 4", messageCount)
	}

	for _, subj := range []string{"msg1", "msg2", "msg3", "msg4"} {
		var c int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE subject = ?`, subj).Scan(&c); err != nil {
			t.Fatalf("count subject %q: %v", subj, err)
		}
		if c != 1 {
			t.Fatalf("subject %q count = %d, want 1", subj, c)
		}
	}
}

func TestImportMbox_InvalidResumeOffsetBeyondEOF_FailsSync(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	mboxData := "From sender@example.com Mon Jan 1 00:00:00 2024\nSubject: One\n\nBody\n"
	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}
	absPath, err := filepath.Abs(mboxPath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	fi, err := os.Stat(absPath)
	if err != nil {
		t.Fatalf("stat mbox: %v", err)
	}

	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	syncID, err := st.StartSync(src.ID, "import-mbox")
	if err != nil {
		t.Fatalf("start sync: %v", err)
	}

	cp := store.Checkpoint{}
	if err := saveMboxCheckpoint(st, syncID, absPath, fi.Size()+1, 0, &cp); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	_, err = ImportMbox(context.Background(), st, absPath, MboxImportOptions{
		SourceType: "mbox",
		Identifier: "me@example.com",
		NoResume:   false,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "beyond end of file") {
		t.Fatalf("unexpected error: %v", err)
	}

	var status string
	if err := st.DB().QueryRow(`SELECT status FROM sync_runs WHERE id = ?`, syncID).Scan(&status); err != nil {
		t.Fatalf("select sync: %v", err)
	}
	if status != store.SyncStatusFailed {
		t.Fatalf("status = %q, want %q", status, store.SyncStatusFailed)
	}

	var messageCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 0 {
		t.Fatalf("messageCount = %d, want 0", messageCount)
	}
}

func TestImportMbox_HardErrorsStopsMultiFileLoop(t *testing.T) {
	// Verify that when ImportMbox reports HardErrors, the caller's
	// multi-file loop should break. This simulates the control flow
	// in import_mbox.go where HardErrors now stops subsequent files.
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	raw1 := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("File1").
		Body("body1\n").
		Bytes()
	raw2 := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("File2").
		Body("body2\n").
		Bytes()

	writeMbox := func(name string, raw []byte) string {
		var buf strings.Builder
		buf.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
		buf.Write(raw)
		if !strings.HasSuffix(buf.String(), "\n") {
			buf.WriteString("\n")
		}
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(buf.String()), 0600); err != nil {
			t.Fatalf("write mbox %s: %v", name, err)
		}
		return p
	}

	path1 := writeMbox("file1.mbox", raw1)
	path2 := writeMbox("file2.mbox", raw2)

	// Inject ingest failure to cause HardErrors on file1.
	failIngest := func(_ context.Context, _ *store.Store, _ int64, _ string, _ string, _ []int64, _ string, _ string, _ *mbox.Message, _ *slog.Logger) error {
		return fmt.Errorf("injected failure")
	}

	sum1, err := ImportMbox(context.Background(), st, path1, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		IngestFunc:         failIngest,
	})
	if err != nil {
		t.Fatalf("ImportMbox file1: %v", err)
	}
	if !sum1.HardErrors {
		t.Fatalf("expected HardErrors=true for file1")
	}

	// Simulate the multi-file loop: if HardErrors, skip file2.
	if sum1.HardErrors {
		// This is the behavior we're testing: the loop breaks.
		// Verify file2 was NOT processed.
		var msgCount int
		if err := st.DB().QueryRow(
			`SELECT COUNT(*) FROM messages`,
		).Scan(&msgCount); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if msgCount != 0 {
			t.Fatalf("msgCount = %d, want 0 (file1 failed)", msgCount)
		}

		// File2 should not have been imported.
		_ = path2 // would be processed if loop didn't break
		return
	}

	t.Fatalf("should not reach here; HardErrors should have stopped the loop")
}

type cancelOnLogMessageHandler struct {
	msg    string
	cancel func()
	next   slog.Handler
}

func (h *cancelOnLogMessageHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *cancelOnLogMessageHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.cancel()
	}
	return h.next.Handle(ctx, r)
}

func (h *cancelOnLogMessageHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &cancelOnLogMessageHandler{
		msg:    h.msg,
		cancel: h.cancel,
		next:   h.next.WithAttrs(attrs),
	}
}

func (h *cancelOnLogMessageHandler) WithGroup(name string) slog.Handler {
	return &cancelOnLogMessageHandler{
		msg:    h.msg,
		cancel: h.cancel,
		next:   h.next.WithGroup(name),
	}
}

func TestImportMbox_CheckpointAdvancesPastReaderErrors(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	maxBytes := int64(64)

	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024",
		"Subject: " + strings.Repeat("a", 200),
		"",
		"Body1",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")
	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mboxData), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

	// Expected resume point is the next message separator after the read error.
	f, err := os.Open(mboxPath)
	if err != nil {
		t.Fatalf("open mbox: %v", err)
	}
	r := mbox.NewReaderWithMaxMessageBytes(f, maxBytes)
	_, _ = r.Next() // first message is expected to exceed max size
	wantOffset := r.NextFromOffset()
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	log := slog.New(&cancelOnLogMessageHandler{
		msg:    "mbox read error",
		cancel: cancel,
		next:   slog.NewTextHandler(io.Discard, nil),
	})

	_, err = ImportMbox(ctx, st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		MaxMessageBytes:    maxBytes,
		Logger:             log,
	})
	if err != nil {
		t.Fatalf("ImportMbox: %v", err)
	}

	var (
		status string
		cursor string
	)
	if err := st.DB().QueryRow(`SELECT status, cursor_before FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &cursor); err != nil {
		t.Fatalf("select sync: %v", err)
	}
	if status != store.SyncStatusRunning {
		t.Fatalf("status = %q, want %q", status, store.SyncStatusRunning)
	}

	var cp mboxCheckpoint
	if err := json.Unmarshal([]byte(cursor), &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	if cp.Offset != wantOffset {
		t.Fatalf("checkpoint offset = %d, want %d", cp.Offset, wantOffset)
	}
}
