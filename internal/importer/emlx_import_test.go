package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

// mkEmlx creates an .emlx file with the given MIME bytes.
func mkEmlx(t *testing.T, dir, name string, raw []byte) {
	t.Helper()
	data := fmt.Sprintf("%d\n%s", len(raw), raw)
	requirepkg.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data), 0600), "write emlx")
}

// mkMailboxDir creates an Apple Mail mailbox directory with .emlx files.
func mkMailboxDir(t *testing.T, base string, emlxFiles map[string][]byte) {
	t.Helper()
	msgDir := filepath.Join(base, "Messages")
	requirepkg.NoError(t, os.MkdirAll(msgDir, 0700), "mkdir")
	for name, raw := range emlxFiles {
		mkEmlx(t, msgDir, name, raw)
	}
}

func openTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	requirepkg.NoError(t, err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	requirepkg.NoError(t, st.InitSchema(), "init schema")
	return st, tmp
}

func TestImportEmlxDir_SingleMailbox(t *testing.T) {
	require := requirepkg.New(t)
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
	require.NoError(err, "ImportEmlxDir")
	require.Equal(int64(2), summary.MessagesAdded, "MessagesAdded")
	require.Equal(1, summary.MailboxesImported, "MailboxesImported")

	var msgCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	require.NoError(err, "count messages")
	require.Equal(2, msgCount, "msgCount")

	// Verify labels were created and assigned.
	var labelCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM labels WHERE name = 'Test'`).Scan(&labelCount)
	require.NoError(err, "count labels")
	require.Equal(1, labelCount, "labelCount")

	var msgLabelCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&msgLabelCount)
	require.NoError(err, "count message_labels")
	require.Equal(2, msgLabelCount, "msgLabelCount")
}

func TestImportEmlxDir_MultiMailboxLabels(t *testing.T) {
	require := requirepkg.New(t)
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
	require.NoError(err, "ImportEmlxDir")

	// Only one message should be created (dedup by content hash).
	require.Equal(int64(1), summary.MessagesAdded, "MessagesAdded")

	// But it should have labels from both mailboxes.
	var msgCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	require.NoError(err, "count messages")
	require.Equal(1, msgCount, "msgCount")

	var labelNames []string
	rows, err := st.DB().Query(`
		SELECT l.name FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		JOIN messages m ON m.id = ml.message_id
		ORDER BY l.name
	`)
	require.NoError(err, "query labels")
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		require.NoError(rows.Scan(&name), "scan label")
		labelNames = append(labelNames, name)
	}
	require.NoError(rows.Err(), "rows error")

	require.Equal([]string{"Archive", "Inbox"}, labelNames)
}

func TestImportEmlxDir_EmptyMailbox(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Empty.mbox")
	requirepkg.NoError(t, os.MkdirAll(filepath.Join(mboxDir, "Messages"), 0700), "mkdir")

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "test@example.com",
			NoResume:   true,
		},
	)
	requirepkg.NoError(t, err, "ImportEmlxDir")
	requirepkg.Equal(t, int64(0), summary.MessagesProcessed, "MessagesProcessed")
}

func TestImportEmlxDir_InvalidEmlxSoftError(t *testing.T) {
	require := requirepkg.New(t)
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Bad.mbox")
	msgDir := filepath.Join(mboxDir, "Messages")
	require.NoError(os.MkdirAll(msgDir, 0700), "mkdir")

	// Create a valid emlx.
	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Good").
		Body("ok\n").
		Bytes()
	mkEmlx(t, msgDir, "1.emlx", raw)

	// Create an invalid emlx.
	require.NoError(os.WriteFile(filepath.Join(msgDir, "2.emlx"), []byte("not-valid"), 0600),
		"write bad emlx")

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           true,
			CheckpointInterval: 1,
		},
	)
	require.NoError(err, "ImportEmlxDir")

	// The valid message should still be imported.
	require.Equal(int64(1), summary.MessagesAdded, "MessagesAdded")
	assertpkg.NotZero(t, summary.Errors, "expected errors > 0")
}

func TestImportEmlxDir_ResumeFromCheckpoint(t *testing.T) {
	require := requirepkg.New(t)
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
	require.NoError(err, "ImportEmlxDir (first)")

	// Second import: resume should skip already-imported messages.
	summary2, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           false,
			CheckpointInterval: 1,
		},
	)
	require.NoError(err, "ImportEmlxDir (resume)")

	// Already-imported messages should be skipped.
	require.Equal(int64(0), summary2.MessagesAdded, "MessagesAdded (resume)")

	var msgCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	require.NoError(err, "count messages")
	require.Equal(2, msgCount, "msgCount")
}

func TestImportEmlxDir_PartialEmlxSkipped(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")
	msgDir := filepath.Join(mboxDir, "Messages")
	requirepkg.NoError(t, os.MkdirAll(msgDir, 0700), "mkdir")

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
	requirepkg.NoError(t, err, "ImportEmlxDir")
	// Only the non-partial file should be imported.
	requirepkg.Equal(t, int64(1), summary.MessagesAdded, "MessagesAdded")
}

func TestImportEmlxDir_NoMailboxes(t *testing.T) {
	st, tmp := openTestStore(t)

	root := filepath.Join(tmp, "Mail")
	requirepkg.NoError(t, os.MkdirAll(root, 0700), "mkdir")

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "test@example.com",
			NoResume:   true,
		},
	)
	requirepkg.NoError(t, err, "ImportEmlxDir")
	requirepkg.Equal(t, 0, summary.MailboxesTotal, "MailboxesTotal")
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
	requirepkg.NoError(t, err, "ImportEmlxDir")
	requirepkg.Equal(t, int64(0), summary.MessagesAdded, "MessagesAdded")
	assertpkg.NotZero(t, summary.Errors, "expected errors > 0 for oversized file")
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
	requirepkg.NoError(t, err, "ImportEmlxDir")

	// Sync run should still be in "running" state (not completed),
	// so resume can pick it up.
	var status string
	err = st.DB().QueryRow(
		`SELECT status FROM sync_runs ORDER BY started_at DESC LIMIT 1`,
	).Scan(&status)
	requirepkg.NoError(t, err, "select sync")
	requirepkg.Equal(t, store.SyncStatusRunning, status)
}

func TestImportEmlxDir_SameMailboxDuplicateFiles(t *testing.T) {
	require := requirepkg.New(t)
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
	require.NoError(err, "ImportEmlxDir")

	require.Equal(int64(1), summary.MessagesAdded, "MessagesAdded")

	var msgCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	require.NoError(err, "count messages")
	require.Equal(1, msgCount, "msgCount")

	// Only one label mapping (Inbox), not duplicated.
	var mlCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&mlCount)
	require.NoError(err, "count message_labels")
	require.Equal(1, mlCount, "message_labels count")
}

func TestImportEmlxDir_Idempotent(t *testing.T) {
	require := requirepkg.New(t)
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
	require.NoError(err, "ImportEmlxDir (first)")

	summary, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "alice@example.com",
			NoResume:   true,
		},
	)
	require.NoError(err, "ImportEmlxDir (second)")

	require.Equal(int64(0), summary.MessagesAdded, "MessagesAdded")

	var msgCount int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount)
	require.NoError(err, "count messages")
	require.Equal(1, msgCount, "msgCount")
}

func TestImportEmlxDir_MailboxPathMismatchRejectsResume(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "abs root")

	// Seed checkpoint with correct root but a different mailbox path
	// at index 0, simulating the mailbox tree changing between runs.
	src, err := st.GetOrCreateSource("apple-mail", "alice@example.com")
	require.NoError(err, "get/create source")
	syncID, err := st.StartSync(src.ID, "import-emlx")
	require.NoError(err, "start sync")
	require.NoError(saveEmlxCheckpoint(
		st, syncID, absRoot, 0,
		"/old/path/to/OtherMailbox.mbox", "",
		&store.Checkpoint{},
	), "save checkpoint")

	_, err = ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           false,
			CheckpointInterval: 1,
		},
	)
	require.Error(err, "expected error for mailbox path mismatch")
	require.ErrorContains(err, "--no-resume")
	assert.ErrorContains(err, "changed")
}

func TestImportEmlxDir_NegativeIndexRejectsResume(t *testing.T) {
	require := requirepkg.New(t)
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
	require.NoError(err, "abs root")

	// Seed checkpoint with MailboxIndex = -1 (corrupted data).
	src, err := st.GetOrCreateSource("apple-mail", "alice@example.com")
	require.NoError(err, "get/create source")
	syncID, err := st.StartSync(src.ID, "import-emlx")
	require.NoError(err, "start sync")
	cpJSON, err := json.Marshal(emlxCheckpoint{
		RootDir:      absRoot,
		MailboxIndex: -1,
		LastFile:     "",
	})
	require.NoError(err, "marshal checkpoint")
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken: string(cpJSON),
	}), "save checkpoint")

	_, err = ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier: "alice@example.com",
			NoResume:   false,
		},
	)
	require.Error(err, "expected error for negative index")
	require.ErrorContains(err, "out of range")
	require.ErrorContains(err, "--no-resume")
}

func TestImportEmlxDir_RootMismatchRejectsResume(t *testing.T) {
	require := requirepkg.New(t)
	st, tmp := openTestStore(t)

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		Subject("Hello").
		Body("hi\n").
		Bytes()

	// Seed an active (running) sync with a checkpoint pointing to root A.
	src, err := st.GetOrCreateSource("apple-mail", "alice@example.com")
	require.NoError(err, "get/create source")
	syncID, err := st.StartSync(src.ID, "import-emlx")
	require.NoError(err, "start sync")
	absRootA, err := filepath.Abs(filepath.Join(tmp, "MailA"))
	require.NoError(err, "abs root A")
	require.NoError(saveEmlxCheckpoint(
		st, syncID, absRootA, 0, "", "", &store.Checkpoint{},
	), "save checkpoint")

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
	require.Error(err, "expected error for root mismatch")
	assertpkg.ErrorContains(t, err, "--no-resume")
}

func TestImportEmlxDir_CheckpointBlockedOnIngestFailure(t *testing.T) {
	require := requirepkg.New(t)
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
			return errors.New("injected failure")
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
	require.NoError(err, "ImportEmlxDir")
	require.True(summary.HardErrors, "expected HardErrors=true")
	// msg1 and msg3 should be ingested; msg2 failed.
	require.Equal(int64(2), summary.MessagesAdded, "MessagesAdded")

	// Verify the checkpoint cursor stayed at msg1 (did not advance
	// past the failed msg2).
	var cursor string
	err = st.DB().QueryRow(
		`SELECT cursor_before FROM sync_runs
		 ORDER BY started_at DESC LIMIT 1`,
	).Scan(&cursor)
	require.NoError(err, "select cursor")
	var cp emlxCheckpoint
	require.NoError(json.Unmarshal([]byte(cursor), &cp), "unmarshal checkpoint")
	require.Equal("1.emlx", filepath.Base(cp.LastFile),
		"checkpoint LastFile should not advance past failed msg2; got %q", cp.LastFile)

	// Resume should retry msg2 and succeed (no injected failure this time).
	summary2, err := ImportEmlxDir(
		context.Background(), st, root, EmlxImportOptions{
			Identifier:         "alice@example.com",
			NoResume:           false,
			CheckpointInterval: 1,
		},
	)
	require.NoError(err, "ImportEmlxDir (resume)")
	// msg2 should now be added; msg1 and msg3 already exist.
	require.Equal(int64(1), summary2.MessagesAdded, "MessagesAdded (resume)")

	var total int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&total)
	require.NoError(err, "count messages")
	require.Equal(3, total, "total messages")
}
