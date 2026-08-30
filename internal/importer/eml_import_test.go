package importer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportEMLDirMergesDuplicateMessageLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate")
	aDir := filepath.Join(root, "A.mailbox")
	bDir := filepath.Join(aDir, "B.mailbox")
	require.NoError(os.MkdirAll(bDir, 0o700))

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Plain EML").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<plain-eml@example.com>").
		Body("Imported from an EML tree.\n").
		Bytes()
	require.NoError(os.WriteFile(filepath.Join(aDir, "first.eml"), raw, 0o600))
	require.NoError(os.WriteFile(filepath.Join(bDir, "duplicate.eml"), raw, 0o600))

	summary, err := ImportEMLDir(context.Background(), st, root, EMLImportOptions{
		Identifier: "alice@example.com",
		NoResume:   true,
	})
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(int64(1), summary.MessagesSkipped)

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
	assert.Equal(1, messageCount)

	rows, err := st.DB().Query(`
		SELECT l.name
		FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		ORDER BY l.name`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var labels []string
	for rows.Next() {
		var label string
		require.NoError(rows.Scan(&label))
		labels = append(labels, label)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"A", "A/B"}, labels)

	var sourceType, rawFormat string
	require.NoError(st.DB().QueryRow(`
		SELECT s.source_type, mr.raw_format
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		JOIN message_raw mr ON mr.message_id = m.id`).Scan(&sourceType, &rawFormat))
	assert.Equal("eml", sourceType)
	assert.Equal("mime", rawFormat)
}

func TestImportEMLDirRequiresIdentifier(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "A.mailbox")
	require.NoError(os.Mkdir(root, 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), []byte("Subject: test\r\n\r\nbody"), 0o600))

	summary, err := ImportEMLDir(context.Background(), st, root, EMLImportOptions{})

	assert.Nil(summary)
	assert.ErrorContains(err, "identifier is required")
}

func TestImportEMLDirReturnsCancellation(t *testing.T) {
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "A.mailbox")
	require.NoError(os.Mkdir(root, 0o700))
	require.NoError(os.WriteFile(
		filepath.Join(root, "message.eml"),
		[]byte("Subject: test\r\n\r\nbody"),
		0o600,
	))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	summary, err := ImportEMLDir(ctx, st, root, EMLImportOptions{
		Identifier: "cancelled@example.test",
	})

	assert.NotNil(t, summary, "cancellation should retain progress for reporting")
	require.ErrorIs(err, context.Canceled)
}

func TestImportEMLDirResumesAfterInterruptedFileBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate")
	mailbox := filepath.Join(root, "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	writeTestEML(t, filepath.Join(mailbox, "1.eml"), "One")
	writeTestEML(t, filepath.Join(mailbox, "2.eml"), "Two")

	ctx, cancel := context.WithCancel(t.Context())
	ingestCalls := 0
	first, err := ImportEMLDir(ctx, st, root, EMLImportOptions{
		Identifier:         "resume@example.test",
		CheckpointInterval: 1,
		IngestFunc: func(
			ctx context.Context, st *store.Store,
			sourceID int64, identifier, attachmentsDir string,
			labelIDs []int64, sourceMsgID, rawHash string,
			raw []byte, fallbackDate time.Time,
			log *slog.Logger,
		) error {
			ingestCalls++
			err := IngestRawMessage(
				ctx, st, sourceID, identifier, attachmentsDir,
				labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log,
			)
			if err == nil && ingestCalls == 1 {
				cancel()
			}
			return err
		},
	})
	require.ErrorIs(err, context.Canceled)
	require.NotNil(first)
	assert.Equal(int64(1), first.MessagesAdded)

	resumed, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier:         "resume@example.test",
		CheckpointInterval: 1,
	})
	require.NoError(err)
	assert.True(resumed.WasResumed)
	assert.Equal(int64(1), resumed.MessagesAdded)
	assert.Equal(int64(1), resumed.MessagesSkipped)
	assert.Equal(int64(2), resumed.MessagesProcessed)

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
	assert.Equal(2, messageCount)
}

func TestImportEMLDirResumesFailedCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate")
	mailbox := filepath.Join(root, "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	writeTestEML(t, filepath.Join(mailbox, "1.eml"), "One")

	first, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier:         "failed-resume@example.test",
		CheckpointInterval: 1,
		IngestFunc: func(
			context.Context, *store.Store,
			int64, string, string, []int64, string, string,
			[]byte, time.Time, *slog.Logger,
		) error {
			return errors.New("injected ingest failure")
		},
	})
	require.NoError(err)
	require.NotNil(first)
	assert.True(first.HardErrors)
	assert.Equal(int64(1), first.Errors)

	resumed, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier:         "failed-resume@example.test",
		CheckpointInterval: 1,
	})
	require.NoError(err)
	assert.True(resumed.WasResumed)
	assert.Equal(int64(1), resumed.MessagesAdded)

	latest, err := st.GetLatestSync(resumed.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
}

func TestImportEMLDirIgnoresIncompatibleCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate")
	mailbox := filepath.Join(root, "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	writeTestEML(t, filepath.Join(mailbox, "1.eml"), "One")

	source, err := st.GetOrCreateSource("eml", "mixed-imports@example.test")
	require.NoError(err)
	foreignSyncID, err := st.StartSync(source.ID, "import-mbox")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(foreignSyncID, &store.Checkpoint{
		PageToken: `{"file":"/tmp/archive.mbox","offset":1024}`,
	}))

	summary, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier: "mixed-imports@example.test",
	})

	require.NoError(err)
	assert.False(summary.WasResumed)
	assert.Equal(int64(1), summary.MessagesAdded)
}

func TestImportEMLDirResumeRescansFilesBeforeCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate")
	mailbox := filepath.Join(root, "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	writeTestEML(t, filepath.Join(mailbox, "2.eml"), "Two")

	ctx, cancel := context.WithCancel(t.Context())
	first, err := ImportEMLDir(ctx, st, root, EMLImportOptions{
		Identifier:         "resume-rescan@example.test",
		CheckpointInterval: 1,
		IngestFunc: func(
			ctx context.Context, st *store.Store,
			sourceID int64, identifier, attachmentsDir string,
			labelIDs []int64, sourceMsgID, rawHash string,
			raw []byte, fallbackDate time.Time,
			log *slog.Logger,
		) error {
			err := IngestRawMessage(
				ctx, st, sourceID, identifier, attachmentsDir,
				labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log,
			)
			if err == nil {
				cancel()
			}
			return err
		},
	})
	require.ErrorIs(err, context.Canceled)
	require.NotNil(first)
	assert.Equal(int64(1), first.MessagesAdded)

	writeTestEML(t, filepath.Join(mailbox, "1.eml"), "One")
	resumed, err := ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier:         "resume-rescan@example.test",
		CheckpointInterval: 1,
	})
	require.NoError(err)
	assert.True(resumed.WasResumed)
	assert.Equal(int64(1), resumed.MessagesAdded,
		"new files before the checkpoint must still be imported")

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
	assert.Equal(2, messageCount)
}

func TestImportEMLDirRejectsChangedMailboxAtCheckpoint(t *testing.T) {
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate")
	mailbox := filepath.Join(root, "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	writeTestEML(t, filepath.Join(mailbox, "1.eml"), "One")

	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	source, err := st.GetOrCreateSource("eml", "changed@example.test")
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, "import-eml")
	require.NoError(err)
	require.NoError(saveEMLCheckpoint(
		st, syncID, absRoot, 0, "/old/Inbox.mailbox", "", &store.Checkpoint{},
	))

	_, err = ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier: "changed@example.test",
	})
	require.Error(err)
	assert.ErrorContains(t, err, "mailbox tree changed")
}

func TestImportEMLDirRejectsNegativeCheckpointIndex(t *testing.T) {
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate")
	mailbox := filepath.Join(root, "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	writeTestEML(t, filepath.Join(mailbox, "1.eml"), "One")

	absRoot, err := filepath.Abs(root)
	require.NoError(err)
	source, err := st.GetOrCreateSource("eml", "negative@example.test")
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, "import-eml")
	require.NoError(err)
	cursor, err := json.Marshal(emlCheckpoint{RootDir: absRoot, MailboxIndex: -1})
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken: string(cursor),
	}))

	_, err = ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier: "negative@example.test",
	})
	require.Error(err)
	assert.ErrorContains(t, err, "out of range")
}

func TestImportEMLDirRejectsCheckpointForDifferentRoot(t *testing.T) {
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "MailMate-B")
	mailbox := filepath.Join(root, "Inbox.mailbox")
	require.NoError(os.MkdirAll(mailbox, 0o700))
	writeTestEML(t, filepath.Join(mailbox, "1.eml"), "One")

	otherRoot, err := filepath.Abs(filepath.Join(tmp, "MailMate-A"))
	require.NoError(err)
	source, err := st.GetOrCreateSource("eml", "root@example.test")
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, "import-eml")
	require.NoError(err)
	require.NoError(saveEMLCheckpoint(
		st, syncID, otherRoot, 0, "", "", &store.Checkpoint{},
	))

	_, err = ImportEMLDir(t.Context(), st, root, EMLImportOptions{
		Identifier: "root@example.test",
	})
	require.Error(err)
	require.ErrorContains(err, "different directory")
	require.ErrorContains(err, "--no-resume")
}

func writeTestEML(t *testing.T, path, subject string) {
	t.Helper()
	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject(subject).
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<"+subject+"@example.test>").
		Body("Imported from an EML tree.\n").
		Bytes()
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}
