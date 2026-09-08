package importer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func maildirFixture(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{"cur", "new", "tmp"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0700))
	}
}

func TestImportMaildirPreservesRawAndMergesFolders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "mail")
	maildirFixture(t, root)
	maildirFixture(t, filepath.Join(root, ".Archive"))
	raw := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Maildir message\r\nMessage-ID: <one@example.com>\r\n\r\nArchive me.\r\n")
	seenName := "one:2,S"
	if runtime.GOOS == "windows" {
		// Colons select alternate data streams on Windows, not regular files.
		seenName = "one-seen"
	}
	for _, path := range []string{"new/one", filepath.Join(".Archive", "cur", seenName)} {
		require.NoError(os.WriteFile(filepath.Join(root, path), raw, 0600))
	}
	require.NoError(os.WriteFile(filepath.Join(root, "tmp", "pending"), []byte("Subject: incomplete\r\n\r\n"), 0600))
	opts := MaildirImportOptions{Identifier: "alice@example.com"}
	summary, err := ImportMaildir(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Equal(int64(1), summary.MessagesSkipped)
	var sourceType, rawFormat string
	require.NoError(st.DB().QueryRow(`SELECT s.source_type, mr.raw_format FROM messages m JOIN sources s ON s.id=m.source_id JOIN message_raw mr ON mr.message_id=m.id`).Scan(&sourceType, &rawFormat))
	assert.Equal("maildir", sourceType)
	assert.Equal("mime", rawFormat)
	var messageID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
	storedRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Equal(raw, storedRaw)
	rows, err := st.DB().Query(`SELECT l.name FROM message_labels ml JOIN labels l ON l.id=ml.label_id ORDER BY l.name`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var labels []string
	for rows.Next() {
		var name string
		require.NoError(rows.Scan(&name))
		labels = append(labels, name)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"Archive", "INBOX", "UNREAD"}, labels)
	require.NoError(os.Rename(filepath.Join(root, "new/one"), filepath.Join(root, "cur", seenName)))
	second, err := ImportMaildir(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Zero(second.MessagesAdded)
	assert.Equal(int64(2), second.MessagesSkipped)
	got, err := os.ReadFile(filepath.Join(root, "cur", seenName))
	require.NoError(err)
	assert.Equal(raw, got)
}

func TestImportMaildirCancellationAndResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "mail")
	maildirFixture(t, root)
	require.NoError(os.WriteFile(filepath.Join(root, "new/one"), []byte("Subject: resume\r\n\r\nbody"), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	first, err := ImportMaildir(ctx, st, root, MaildirImportOptions{Identifier: "alice@example.com"})
	require.ErrorIs(err, context.Canceled)
	require.NotNil(first)
	second, err := ImportMaildir(t.Context(), st, root, MaildirImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.True(second.WasResumed)
	assert.Equal(int64(1), second.MessagesAdded)
}

func TestImportMaildirEmptyAndOversized(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "mail")
	maildirFixture(t, root)
	opts := MaildirImportOptions{Identifier: "alice@example.com", MaxMessageBytes: 16}
	empty, err := ImportMaildir(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Zero(empty.MessagesAdded)
	require.NoError(os.WriteFile(filepath.Join(root, "new/large"), []byte("Subject: too large\r\n\r\nthis message exceeds the limit"), 0600))
	result, err := ImportMaildir(t.Context(), st, root, opts)
	require.NoError(err)
	assert.True(result.HardErrors)
	assert.Equal(int64(1), result.Errors)
	assert.Zero(result.MessagesAdded)
}

func TestImportMaildirStoresAttachment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "mail")
	maildirFixture(t, root)
	raw := email.NewMessage().WithAttachment("sample.txt", "text/plain", []byte("synthetic attachment")).Bytes()
	require.NoError(os.WriteFile(filepath.Join(root, "cur/message"), raw, 0600))
	attachments := filepath.Join(tmp, "attachments")
	result, err := ImportMaildir(t.Context(), st, root, MaildirImportOptions{Identifier: "alice@example.com", AttachmentsDir: attachments})
	require.NoError(err)
	assert.Equal(int64(1), result.MessagesAdded)
	var path string
	require.NoError(st.DB().QueryRow(`SELECT storage_path FROM attachments`).Scan(&path))
	stored, err := os.ReadFile(filepath.Join(attachments, path))
	require.NoError(err)
	assert.Equal([]byte("synthetic attachment"), stored)
}

func TestImportMaildirRecoveredFailureIsSuccessful(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "mail")
	maildirFixture(t, root)
	require.NoError(os.WriteFile(filepath.Join(root, "new/one"), []byte("Subject: retry\r\n\r\nbody"), 0600))
	_, err := st.DB().Exec(`CREATE TRIGGER fail_maildir_insert BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'transient import failure'); END`)
	require.NoError(err)
	opts := MaildirImportOptions{Identifier: "alice@example.com"}
	first, err := ImportMaildir(t.Context(), st, root, opts)
	require.NoError(err)
	require.True(first.HardErrors)
	assert.Equal(int64(1), first.Errors)
	_, err = st.DB().Exec(`DROP TRIGGER fail_maildir_insert`)
	require.NoError(err)
	recovered, err := ImportMaildir(t.Context(), st, root, opts)
	require.NoError(err)
	require.True(recovered.WasResumed)
	assert.Equal(int64(1), recovered.MessagesAdded)
	run, err := st.GetLatestSync(recovered.SourceID)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	assert.Zero(run.ErrorsCount, "a fully recovered scan must be classified as successful")
}

func TestImportMaildirFolderNamedLikeFlagLabel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "mail")
	maildirFixture(t, root)
	maildirFixture(t, filepath.Join(root, ".TRASH"))
	trashedName := "one:2,T"
	if runtime.GOOS == "windows" {
		trashedName = "one-trashed"
	}
	raw := []byte("From: alice@example.com\r\nSubject: trashed\r\n\r\nbody\r\n")
	require.NoError(os.WriteFile(filepath.Join(root, ".TRASH", "cur", trashedName), raw, 0600))
	summary, err := ImportMaildir(t.Context(), st, root, MaildirImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Zero(summary.Errors)
	assert.False(summary.HardErrors)
	rows, err := st.DB().Query(`SELECT l.name FROM message_labels ml JOIN labels l ON l.id=ml.label_id ORDER BY l.name`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var labels []string
	for rows.Next() {
		var name string
		require.NoError(rows.Scan(&name))
		labels = append(labels, name)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"TRASH", "UNREAD"}, labels)
}
