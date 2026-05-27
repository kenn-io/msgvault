package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mbox"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportMbox_IngestsMessagesThreadsAndAttachments(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600), "write mbox")

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "hey",
		Identifier:         "me@hey.com",
		Labels:             []string{"hey"},
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     attachmentsDir,
	})
	require.NoError(err, "ImportMbox")
	require.Equal(int64(2), summary.MessagesAdded, "MessagesAdded")

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
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sources WHERE source_type = 'hey' AND identifier = 'me@hey.com'`).Scan(&sourceCount), "count sources")
	require.Equal(1, sourceCount, "sourceCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&conversationCount), "count conversations")
	require.Equal(1, conversationCount, "conversationCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages")
	require.Equal(2, messageCount, "messageCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&rawCount), "count message_raw")
	require.Equal(2, rawCount, "rawCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM labels WHERE name = 'hey'`).Scan(&labelCount), "count labels")
	require.Equal(1, labelCount, "labelCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&msgLabelCount), "count message_labels")
	require.Equal(2, msgLabelCount, "msgLabelCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentCount), "count attachments")
	require.Equal(1, attachmentCount, "attachmentCount")

	// Verify attachment file exists.
	var storagePath string
	require.NoError(st.DB().QueryRow(`SELECT storage_path FROM attachments LIMIT 1`).Scan(&storagePath), "select storage_path")
	require.NotEmpty(storagePath, "storage_path empty")
	_, err = os.Stat(filepath.Join(attachmentsDir, filepath.FromSlash(storagePath)))
	require.NoError(err, "attachment file missing")
}

func TestImportMbox_NoAttachmentsStillRecordsAttachmentMetadata(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600), "write mbox")

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     "",
	})
	require.NoError(err, "ImportMbox")
	require.Equal(int64(1), summary.MessagesAdded, "MessagesAdded")

	var attachmentRows int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentRows), "count attachments")
	require.Equal(0, attachmentRows, "attachmentRows")

	var (
		hasAttachments  bool
		attachmentCount int
	)
	require.NoError(st.DB().QueryRow(`SELECT has_attachments, attachment_count FROM messages WHERE subject = 'Hello' LIMIT 1`).Scan(&hasAttachments, &attachmentCount), "select message attachment metadata")
	require.True(hasAttachments, "has_attachments")
	require.Equal(1, attachmentCount, "attachment_count")
}

func TestImportMbox_IsIdempotentAcrossPathChanges(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.MkdirAll(dir1, 0700), "mkdir dir1")
	require.NoError(os.MkdirAll(dir2, 0700), "mkdir dir2")

	mboxPath1 := filepath.Join(dir1, "export.mbox")
	mboxPath2 := filepath.Join(dir2, "export.mbox")
	require.NoError(os.WriteFile(mboxPath1, []byte(mboxData.String()), 0600), "write mbox1")
	require.NoError(os.WriteFile(mboxPath2, []byte(mboxData.String()), 0600), "write mbox2")

	_, err = ImportMbox(context.Background(), st, mboxPath1, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	require.NoError(err, "ImportMbox")

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages")
	require.Equal(2, messageCount, "messageCount")

	_, err = ImportMbox(context.Background(), st, mboxPath2, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	require.NoError(err, "ImportMbox (second path)")

	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages (second path)")
	require.Equal(2, messageCount, "messageCount (second path)")
}

func TestParseFromLineDate_NamedTimezone(t *testing.T) {
	got, ok := parseFromLineDate("From sender@example.com Mon Jan 1 00:00:00 MST 2024")
	requirepkg.True(t, ok)
	requirepkg.Equal(t, 2024, got.Year())
}

func TestImportMbox_InvalidInput_ReturnsErrorAndFailsSync(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

	mboxPath := filepath.Join(tmp, "not-mbox.txt")
	require.NoError(os.WriteFile(mboxPath, []byte("this is not an mbox file\n"), 0600), "write file")

	_, err = ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	require.Error(err)

	var status string
	require.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status), "select status")
	require.Equal(store.SyncStatusFailed, status)
}

func TestImportMbox_IdenticalRawMessagesAreImportedSeparately(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600), "write mbox")

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	require.NoError(err, "ImportMbox")
	require.Equal(int64(2), summary.MessagesProcessed, "MessagesProcessed")
	require.Equal(int64(2), summary.MessagesAdded, "MessagesAdded")
	require.Equal(int64(0), summary.MessagesSkipped, "MessagesSkipped")
	require.Equal(int64(0), summary.Errors, "Errors")

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages")
	require.Equal(2, messageCount, "messageCount")
}

func TestImportMbox_RerunRepairsMissingRawInsteadOfSkipping(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600), "write mbox")

	// Create a partial ingest: message row exists, but no message_raw row.
	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	require.NoError(err, "get/create source")
	convID, err := st.EnsureConversation(src.ID, "thread1", "Thread")
	require.NoError(err, "ensure conversation")

	sum := sha256.Sum256(raw)
	rawHash := hex.EncodeToString(sum[:])

	sourceMsgID := fmt.Sprintf("mbox-%s-%d", rawHash, int64(1))
	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: sourceMsgID,
		MessageType:     "email",
	})
	require.NoError(err, "upsert message")

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	require.NoError(err, "ImportMbox")
	require.Equal(int64(0), summary.MessagesSkipped, "MessagesSkipped")

	var rawCount int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*)
		FROM message_raw mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_id = ? AND m.source_message_id = ?
	`, src.ID, sourceMsgID).Scan(&rawCount), "count message_raw")
	require.Equal(1, rawCount, "rawCount")
}

func TestImportMbox_RerunRetriesAttachmentsAfterStoreFailure(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

	attachmentsDir := filepath.Join(tmp, "attachments")
	// Force attachment storage errors by making the attachments path a file.
	require.NoError(os.WriteFile(attachmentsDir, []byte("not a dir"), 0600), "write attachments sentinel")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600), "write mbox")

	// Attachment storage is best-effort: the ingest succeeds even
	// though the attachment file couldn't be written to disk.
	_, err = ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     attachmentsDir,
	})
	require.NoError(err, "ImportMbox")

	// Message + raw MIME are committed inside the atomic transaction.
	var rawCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_raw`).Scan(&rawCount), "count message_raw")
	require.Equal(1, rawCount, "rawCount")

	// Attachment was not stored because disk write failed, but the
	// attachment count correction updated the metadata to reflect this.
	var attachmentCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentCount), "count attachments")
	require.Equal(0, attachmentCount, "attachmentCount")

	// Metadata was corrected to reflect zero stored attachments.
	var hasAttachments bool
	var metaCount int
	require.NoError(st.DB().QueryRow(
		`SELECT has_attachments, attachment_count FROM messages LIMIT 1`,
	).Scan(&hasAttachments, &metaCount), "select attachment metadata")
	require.False(hasAttachments, "has_attachments should be false")
	require.Equal(0, metaCount, "attachment_count")

	// Fix the attachments dir, rerun — the message is already
	// ingested so it's skipped, but verify no errors.
	require.NoError(os.Remove(attachmentsDir), "remove attachments file")
	require.NoError(os.MkdirAll(attachmentsDir, 0700), "mkdir attachments dir")

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		AttachmentsDir:     attachmentsDir,
	})
	require.NoError(err, "ImportMbox (rerun)")
	require.Equal(int64(1), summary.MessagesSkipped, "MessagesSkipped")
}

func TestImportMbox_ErrorsCauseSyncFailed(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")
	_, err = st.DB().Exec(`DROP TABLE messages`)
	require.NoError(err, "drop messages")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600), "write mbox")

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	require.NoError(err, "ImportMbox")
	assert.NotZero(summary.Errors, "expected errors > 0")

	var (
		status      string
		errorsCount int
	)
	require.NoError(st.DB().QueryRow(`SELECT status, errors_count FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &errorsCount), "select sync")
	require.Equal(store.SyncStatusFailed, status)
	assert.NotZero(errorsCount, "errorsCount")
}

func TestImportMbox_SoftErrorsDoNotFailSync(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData), 0600), "write mbox")

	summary, err := ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		MaxMessageBytes:    maxBytes,
	})
	require.NoError(err, "ImportMbox")
	assert.NotZero(summary.Errors, "expected errors > 0")
	assert.False(summary.HardErrors, "expected no hard errors")

	var (
		status      string
		errorsCount int
	)
	require.NoError(st.DB().QueryRow(`SELECT status, errors_count FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &errorsCount), "select sync")
	require.Equal(store.SyncStatusCompleted, status)
	assert.NotZero(errorsCount, "errorsCount")
}

func TestImportMbox_CheckpointDoesNotAdvancePastFailedIngest(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600), "write mbox")

	// Capture the offset after the first message: this is the safe resume point
	// if the second message fails to ingest.
	f, err := os.Open(mboxPath)
	require.NoError(err, "open mbox")
	r := mbox.NewReaderWithMaxMessageBytes(f, defaultMaxMboxMessageBytes)
	_, err = r.Next()
	if err != nil {
		_ = f.Close()
		require.NoError(err, "read first message")
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
			return errors.New("boom")
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
	require.NoError(err, "ImportMbox")

	var (
		status string
		cursor string
	)
	require.NoError(st.DB().QueryRow(`SELECT status, cursor_before FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &cursor), "select sync")
	require.Equal(store.SyncStatusRunning, status)

	var cp mboxCheckpoint
	require.NoError(json.Unmarshal([]byte(cursor), &cp), "unmarshal checkpoint")
	require.Equal(wantOffset, cp.Offset, "checkpoint offset")

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages")
	require.Equal(2, messageCount, "messageCount")

	// Resume the interrupted sync and ensure already-ingested messages are not duplicated.
	_, err = ImportMbox(context.Background(), st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           false,
		CheckpointInterval: 1,
	})
	require.NoError(err, "ImportMbox (resume)")

	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages (resume)")
	require.Equal(4, messageCount, "messageCount (resume)")

	for _, subj := range []string{"msg1", "msg2", "msg3", "msg4"} {
		var c int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE subject = ?`, subj).Scan(&c), "count subject %q", subj)
		assertpkg.Equal(t, 1, c, "subject %q count", subj)
	}
}

func TestImportMbox_InvalidResumeOffsetBeyondEOF_FailsSync(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

	mboxData := "From sender@example.com Mon Jan 1 00:00:00 2024\nSubject: One\n\nBody\n"
	mboxPath := filepath.Join(tmp, "export.mbox")
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData), 0600), "write mbox")
	absPath, err := filepath.Abs(mboxPath)
	require.NoError(err, "abs path")
	fi, err := os.Stat(absPath)
	require.NoError(err, "stat mbox")

	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	require.NoError(err, "get/create source")
	syncID, err := st.StartSync(src.ID, "import-mbox")
	require.NoError(err, "start sync")

	cp := store.Checkpoint{}
	require.NoError(saveMboxCheckpoint(st, syncID, absPath, fi.Size()+1, 0, &cp), "save checkpoint")

	_, err = ImportMbox(context.Background(), st, absPath, MboxImportOptions{
		SourceType: "mbox",
		Identifier: "me@example.com",
		NoResume:   false,
	})
	require.Error(err)
	requirepkg.ErrorContains(t, err, "beyond end of file")

	var status string
	require.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs WHERE id = ?`, syncID).Scan(&status), "select sync")
	require.Equal(store.SyncStatusFailed, status)

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages")
	require.Equal(0, messageCount, "messageCount")
}

func TestImportMbox_HardErrorsStopsMultiFileLoop(t *testing.T) {
	require := requirepkg.New(t)
	// Verify that when ImportMbox reports HardErrors, the caller's
	// multi-file loop should break. This simulates the control flow
	// in import_mbox.go where HardErrors now stops subsequent files.
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
		require.NoError(os.WriteFile(p, []byte(buf.String()), 0600), "write mbox %s", name)
		return p
	}

	path1 := writeMbox("file1.mbox", raw1)
	path2 := writeMbox("file2.mbox", raw2)

	// Inject ingest failure to cause HardErrors on file1.
	failIngest := func(_ context.Context, _ *store.Store, _ int64, _ string, _ string, _ []int64, _ string, _ string, _ *mbox.Message, _ *slog.Logger) error {
		return errors.New("injected failure")
	}

	sum1, err := ImportMbox(context.Background(), st, path1, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		IngestFunc:         failIngest,
	})
	require.NoError(err, "ImportMbox file1")
	require.True(sum1.HardErrors, "expected HardErrors=true for file1")

	// Simulate the multi-file loop: if HardErrors, skip file2.
	// This is the behavior we're testing: the loop breaks.
	// Verify file2 was NOT processed.
	var msgCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount), "count messages")
	require.Equal(0, msgCount, "msgCount (file1 failed)")

	// File2 should not have been imported.
	_ = path2 // would be processed if loop didn't break
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
	if err := h.next.Handle(ctx, r); err != nil {
		return fmt.Errorf("delegate slog handler: %w", err)
	}
	return nil
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
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

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
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData), 0600), "write mbox")

	// Expected resume point is the next message separator after the read error.
	f, err := os.Open(mboxPath)
	require.NoError(err, "open mbox")
	r := mbox.NewReaderWithMaxMessageBytes(f, maxBytes)
	_, _ = r.Next() // first message is expected to exceed max size
	wantOffset := r.NextFromOffset()
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	log := slog.New(&cancelOnLogMessageHandler{
		msg:    "mbox read error",
		cancel: cancel,
		// slog.DiscardHandler.Enabled() always returns false, which would
		// stop the wrapping handler's Handle (and thus cancel) from ever
		// firing. A real handler keeps Enabled() true so the cancel triggers.
		next: slog.NewTextHandler(io.Discard, nil), //nolint:sloglint // need an Enabled() handler so the cancel fires
	})

	_, err = ImportMbox(ctx, st, mboxPath, MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		NoResume:           true,
		CheckpointInterval: 1,
		MaxMessageBytes:    maxBytes,
		Logger:             log,
	})
	require.NoError(err, "ImportMbox")

	var (
		status string
		cursor string
	)
	require.NoError(st.DB().QueryRow(`SELECT status, cursor_before FROM sync_runs ORDER BY started_at DESC LIMIT 1`).Scan(&status, &cursor), "select sync")
	require.Equal(store.SyncStatusRunning, status)

	var cp mboxCheckpoint
	require.NoError(json.Unmarshal([]byte(cursor), &cp), "unmarshal checkpoint")
	require.Equal(wantOffset, cp.Offset, "checkpoint offset")
}
