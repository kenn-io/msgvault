package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

// mkEmlx creates an .emlx file with the given MIME bytes.
func mkEmlx(t *testing.T, dir, name string, raw []byte) {
	t.Helper()
	data := fmt.Sprintf("%d\n%s", len(raw), raw)
	if err := os.WriteFile(
		filepath.Join(dir, name), []byte(data), 0600,
	); err != nil {
		t.Fatalf("write emlx: %v", err)
	}
}

// mkMailboxDir creates an Apple Mail mailbox directory with .emlx files.
func mkMailboxDir(
	t *testing.T, base string, emlxFiles map[string][]byte,
) {
	t.Helper()
	msgDir := filepath.Join(base, "Messages")
	if err := os.MkdirAll(msgDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, raw := range emlxFiles {
		mkEmlx(t, msgDir, name, raw)
	}
}

func openTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
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
	return st, tmp
}

func TestImportEmlxDir_SingleMailbox(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")

	raw1 := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		Bytes()

	raw2 := email.NewMessage().
		From("Bob <bob@example.com>").
		To("Alice <alice@example.com>").
		Subject("Re: Hello").
		Date("Mon, 01 Jan 2024 13:00:00 +0000").
		Header("Message-ID", "<msg2@example.com>").
		Header("In-Reply-To", "<msg1@example.com>").
		Body("Reply.\n").
		Bytes()

	mkMailboxDir(t, mboxDir, map[string][]byte{
		"1.emlx": raw1,
		"2.emlx": raw2,
	})

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}
	if summary.MessagesAdded != 2 {
		t.Fatalf("MessagesAdded = %d, want 2", summary.MessagesAdded)
	}
	if summary.MailboxesImported != 1 {
		t.Fatalf(
			"MailboxesImported = %d, want 1",
			summary.MailboxesImported,
		)
	}

	var msgCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 2 {
		t.Fatalf("msgCount = %d, want 2", msgCount)
	}

	// Verify labels were created and assigned.
	var labelCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM labels WHERE name = 'Test'`,
	).Scan(&labelCount); err != nil {
		t.Fatalf("count labels: %v", err)
	}
	if labelCount != 1 {
		t.Fatalf("labelCount = %d, want 1", labelCount)
	}

	var msgLabelCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_labels`,
	).Scan(&msgLabelCount); err != nil {
		t.Fatalf("count message_labels: %v", err)
	}
	if msgLabelCount != 2 {
		t.Fatalf("msgLabelCount = %d, want 2", msgLabelCount)
	}
}

func TestImportEmlxDir_MultiMailboxLabels(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		Bytes()

	// Same message in two different mailboxes.
	mkMailboxDir(t, filepath.Join(root, "Mailboxes", "Inbox.mbox"),
		map[string][]byte{"1.emlx": raw})
	mkMailboxDir(t, filepath.Join(root, "Mailboxes", "Archive.mbox"),
		map[string][]byte{"1.emlx": raw})

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}

	// Only one message should be created (dedup by content hash).
	if summary.MessagesAdded != 1 {
		t.Fatalf("MessagesAdded = %d, want 1", summary.MessagesAdded)
	}

	// But it should have labels from both mailboxes.
	var msgCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("msgCount = %d, want 1", msgCount)
	}

	var labelNames []string
	rows, err := st.DB().Query(`
		SELECT l.name FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		JOIN messages m ON m.id = ml.message_id
		ORDER BY l.name
	`)
	if err != nil {
		t.Fatalf("query labels: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan label: %v", err)
		}
		labelNames = append(labelNames, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}

	if len(labelNames) != 2 {
		t.Fatalf("labels = %v, want 2 labels", labelNames)
	}
	if labelNames[0] != "Archive" || labelNames[1] != "Inbox" {
		t.Fatalf(
			"labels = %v, want [Archive, Inbox]",
			labelNames,
		)
	}
}

func TestImportEmlxDir_EmptyMailbox(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Empty.mbox")
	if err := os.MkdirAll(
		filepath.Join(mboxDir, "Messages"), 0700,
	); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "test@example.com",
			NoResume:   true,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}
	if summary.MessagesProcessed != 0 {
		t.Fatalf(
			"MessagesProcessed = %d, want 0",
			summary.MessagesProcessed,
		)
	}
}

func TestImportEmlxDir_InvalidEmlxSoftError(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Bad.mbox")
	msgDir := filepath.Join(mboxDir, "Messages")
	if err := os.MkdirAll(msgDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create a valid emlx.
	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Good").
		Body("ok\n").
		Bytes()
	mkEmlx(t, msgDir, "1.emlx", raw)

	// Create an invalid emlx.
	if err := os.WriteFile(
		filepath.Join(msgDir, "2.emlx"), []byte("not-valid"), 0600,
	); err != nil {
		t.Fatalf("write bad emlx: %v", err)
	}

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}

	// The valid message should still be imported.
	if summary.MessagesAdded != 1 {
		t.Fatalf("MessagesAdded = %d, want 1", summary.MessagesAdded)
	}
	if summary.Errors == 0 {
		t.Fatalf("expected errors > 0")
	}
}

func TestImportEmlxDir_ResumeFromCheckpoint(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")

	raw1 := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("One").
		Body("first\n").
		Bytes()
	raw2 := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Two").
		Body("second\n").
		Bytes()

	mkMailboxDir(t, mboxDir, map[string][]byte{
		"1.emlx": raw1,
		"2.emlx": raw2,
	})

	// First import: run to completion.
	_, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir (first): %v", err)
	}

	// Second import: resume should skip already-imported messages.
	summary2, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           false,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir (resume): %v", err)
	}

	// Already-imported messages should be skipped.
	if summary2.MessagesAdded != 0 {
		t.Fatalf(
			"MessagesAdded (resume) = %d, want 0",
			summary2.MessagesAdded,
		)
	}

	var msgCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 2 {
		t.Fatalf("msgCount = %d, want 2", msgCount)
	}
}

func TestImportEmlxDir_PartialEmlxSkipped(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")
	msgDir := filepath.Join(mboxDir, "Messages")
	if err := os.MkdirAll(msgDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()
	mkEmlx(t, msgDir, "1.emlx", raw)

	// Create a .partial.emlx file.
	mkEmlx(t, msgDir, "2.partial.emlx", raw)

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}
	// Only the non-partial file should be imported.
	if summary.MessagesAdded != 1 {
		t.Fatalf("MessagesAdded = %d, want 1", summary.MessagesAdded)
	}
}

func TestImportEmlxDir_NoMailboxes(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "test@example.com",
			NoResume:   true,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}
	if summary.MailboxesTotal != 0 {
		t.Fatalf("MailboxesTotal = %d, want 0", summary.MailboxesTotal)
	}
}

func TestImportEmlxDir_OversizedFileRejected(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()
	mkMailboxDir(t, mboxDir, map[string][]byte{"1.emlx": raw})

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
			MaxMessageBytes:    10, // Tiny limit to trigger rejection.
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}
	if summary.MessagesAdded != 0 {
		t.Fatalf("MessagesAdded = %d, want 0", summary.MessagesAdded)
	}
	if summary.Errors == 0 {
		t.Fatalf("expected errors > 0 for oversized file")
	}
}

func TestImportEmlxDir_CancelledLeavesRunning(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()
	mkMailboxDir(t, mboxDir, map[string][]byte{"1.emlx": raw})

	// Cancel context before starting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ImportEmlxDir(ctx, st, root, EmlxImportOptions{
		Identifier:         "alice@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}

	// Sync run should still be in "running" state (not completed),
	// so resume can pick it up.
	var status string
	if err := st.DB().QueryRow(
		`SELECT status FROM sync_runs ORDER BY started_at DESC LIMIT 1`,
	).Scan(&status); err != nil {
		t.Fatalf("select sync: %v", err)
	}
	if status != store.SyncStatusRunning {
		t.Fatalf("status = %q, want %q", status, store.SyncStatusRunning)
	}
}

func TestImportEmlxDir_SameMailboxDuplicateFiles(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Inbox.mbox")

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		Bytes()

	// Two different filenames with identical MIME content in one mailbox.
	mkMailboxDir(t, mboxDir, map[string][]byte{
		"1.emlx": raw,
		"2.emlx": raw,
	})

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}

	if summary.MessagesAdded != 1 {
		t.Fatalf("MessagesAdded = %d, want 1", summary.MessagesAdded)
	}

	var msgCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("msgCount = %d, want 1", msgCount)
	}

	// Only one label mapping (Inbox), not duplicated.
	var mlCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_labels`,
	).Scan(&mlCount); err != nil {
		t.Fatalf("count message_labels: %v", err)
	}
	if mlCount != 1 {
		t.Fatalf("message_labels count = %d, want 1", mlCount)
	}
}

func TestImportEmlxDir_Idempotent(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()
	mkMailboxDir(t, mboxDir, map[string][]byte{"1.emlx": raw})

	_, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "alice@example.com",
			NoResume:   true,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir (first): %v", err)
	}

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "alice@example.com",
			NoResume:   true,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir (second): %v", err)
	}

	if summary.MessagesAdded != 0 {
		t.Fatalf("MessagesAdded = %d, want 0", summary.MessagesAdded)
	}

	var msgCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("msgCount = %d, want 1", msgCount)
	}
}

func TestImportEmlxDir_MailboxPathMismatchRejectsResume(t *testing.T) {
	st, tmp := openTestStore(t)

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()

	// Create root with a single mailbox at index 0.
	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Inbox.mbox")
	mkMailboxDir(t, mboxDir, map[string][]byte{"1.emlx": raw})

	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}

	// Seed checkpoint with correct root but a different mailbox path
	// at index 0, simulating the mailbox tree changing between runs.
	src, err := st.GetOrCreateSource("apple-mail", "alice@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	syncID, err := st.StartSync(src.ID, "import-emlx")
	if err != nil {
		t.Fatalf("start sync: %v", err)
	}
	if err := saveEmlxCheckpoint(
		st, syncID, absRoot, 0,
		"/old/path/to/OtherMailbox.mbox", "",
		&store.Checkpoint{},
	); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	_, err = ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           false,
			CheckpointInterval: 1,
		},
	)
	if err == nil {
		t.Fatalf("expected error for mailbox path mismatch")
	}
	if !strings.Contains(err.Error(), "--no-resume") {
		t.Fatalf(
			"error should mention --no-resume, got: %v", err,
		)
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Fatalf(
			"error should mention 'changed', got: %v", err,
		)
	}
}

func TestImportEmlxDir_NegativeIndexRejectsResume(t *testing.T) {
	st, tmp := openTestStore(t)

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Inbox.mbox")
	mkMailboxDir(t, mboxDir, map[string][]byte{"1.emlx": raw})

	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}

	// Seed checkpoint with MailboxIndex = -1 (corrupted data).
	src, err := st.GetOrCreateSource("apple-mail", "alice@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	syncID, err := st.StartSync(src.ID, "import-emlx")
	if err != nil {
		t.Fatalf("start sync: %v", err)
	}
	cpJSON, _ := json.Marshal(emlxCheckpoint{
		RootDir:      absRoot,
		MailboxIndex: -1,
		LastFile:     "",
	})
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken: string(cpJSON),
	}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	_, err = ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "alice@example.com",
			NoResume:   false,
		},
	)
	if err == nil {
		t.Fatalf("expected error for negative index")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("error should mention out of range, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--no-resume") {
		t.Fatalf(
			"error should mention --no-resume, got: %v", err,
		)
	}
}

func TestImportEmlxDir_RootMismatchRejectsResume(t *testing.T) {
	st, tmp := openTestStore(t)

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()

	// Seed an active (running) sync with a checkpoint pointing to root A.
	src, err := st.GetOrCreateSource("apple-mail", "alice@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	syncID, err := st.StartSync(src.ID, "import-emlx")
	if err != nil {
		t.Fatalf("start sync: %v", err)
	}
	absRootA, err := filepath.Abs(filepath.Join(tmp, "MailA"))
	if err != nil {
		t.Fatalf("abs root A: %v", err)
	}
	if err := saveEmlxCheckpoint(
		st, syncID, absRootA, 0, "", "", &store.Checkpoint{},
	); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// Create a mailbox at root B.
	rootB := filepath.Join(tmp, "MailB")
	mboxB := filepath.Join(rootB, "Mailboxes", "Other.mbox")
	mkMailboxDir(t, mboxB, map[string][]byte{"1.emlx": raw})

	// Attempt import from root B without --no-resume.
	_, err = ImportEmlxDir(
		context.Background(), st, rootB, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           false,
			CheckpointInterval: 1,
		},
	)
	if err == nil {
		t.Fatalf("expected error for root mismatch")
	}
	if !strings.Contains(err.Error(), "--no-resume") {
		t.Fatalf(
			"error should mention --no-resume, got: %v", err,
		)
	}
}

func TestImportEmlxDir_CheckpointBlockedOnIngestFailure(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")

	mkMsg := func(subject string) []byte {
		return email.NewMessage().
			From("Alice <alice@example.com>").
			To("Bob <bob@example.com>").
			Subject(subject).
			Date("Mon, 01 Jan 2024 12:00:00 +0000").
			Header("Message-ID",
				fmt.Sprintf("<%s@example.com>", subject)).
			Body("Hi.\n").
			Bytes()
	}

	mkMailboxDir(t, mboxDir, map[string][]byte{
		"1.emlx": mkMsg("msg1"),
		"2.emlx": mkMsg("msg2"),
		"3.emlx": mkMsg("msg3"),
	})

	// Inject failure on the second message.
	calls := 0
	injectFn := func(
		ctx context.Context, s *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error {
		calls++
		if calls == 2 {
			return fmt.Errorf("injected failure")
		}
		return IngestRawMessage(
			ctx, s, sourceID, identifier, attachmentsDir,
			labelIDs, sourceMsgID, rawHash,
			raw, fallbackDate, log,
		)
	}

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
			IngestFunc:         injectFn,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir: %v", err)
	}
	if !summary.HardErrors {
		t.Fatalf("expected HardErrors=true")
	}
	// msg1 and msg3 should be ingested; msg2 failed.
	if summary.MessagesAdded != 2 {
		t.Fatalf("MessagesAdded = %d, want 2", summary.MessagesAdded)
	}

	// Verify the checkpoint cursor stayed at msg1 (did not advance
	// past the failed msg2).
	var cursor string
	if err := st.DB().QueryRow(
		`SELECT cursor_before FROM sync_runs
		 ORDER BY started_at DESC LIMIT 1`,
	).Scan(&cursor); err != nil {
		t.Fatalf("select cursor: %v", err)
	}
	var cp emlxCheckpoint
	if err := json.Unmarshal([]byte(cursor), &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	if filepath.Base(cp.LastFile) != "1.emlx" {
		t.Fatalf(
			"checkpoint LastFile = %q, want basename %q (should not advance past failed msg2)",
			cp.LastFile, "1.emlx",
		)
	}

	// Resume should retry msg2 and succeed (no injected failure this time).
	summary2, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           false,
			CheckpointInterval: 1,
		},
	)
	if err != nil {
		t.Fatalf("ImportEmlxDir (resume): %v", err)
	}
	// msg2 should now be added; msg1 and msg3 already exist.
	if summary2.MessagesAdded != 1 {
		t.Fatalf(
			"MessagesAdded (resume) = %d, want 1",
			summary2.MessagesAdded,
		)
	}

	var total int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages`,
	).Scan(&total); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if total != 3 {
		t.Fatalf("total messages = %d, want 3", total)
	}
}
