package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/importer/mboxzip"
	"go.kenn.io/msgvault/internal/mbox"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportMboxCmd_EndToEnd_MboxFile(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	// Save/restore global state for cmd package.
	prevCfg := cfg
	prevLogger := logger
	prevSourceType := importMboxSourceType
	prevLabel := importMboxLabels
	prevNoResume := importMboxNoResume
	prevCheckpointInterval := importMboxCheckpointInterval
	prevNoAttachments := importMboxNoAttachments
	prevCfgFile := cfgFile
	prevHomeDir := homeDir
	prevVerbose := verbose
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		cfg = prevCfg
		logger = prevLogger
		importMboxSourceType = prevSourceType
		importMboxLabels = prevLabel
		importMboxNoResume = prevNoResume
		importMboxCheckpointInterval = prevCheckpointInterval
		importMboxNoAttachments = prevNoAttachments
		cfgFile = prevCfgFile
		homeDir = prevHomeDir
		verbose = prevVerbose
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	})

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

	var mbox strings.Builder
	mbox.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mbox.Write(raw1)
	if !strings.HasSuffix(string(raw1), "\n") {
		mbox.WriteString("\n")
	}
	mbox.WriteString("From bob@example.com Mon Jan 1 13:00:00 2024\n")
	mbox.Write(raw2)
	if !strings.HasSuffix(string(raw2), "\n") {
		mbox.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	require.NoError(os.WriteFile(mboxPath, []byte(mbox.String()), 0600), "write mbox")

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-mbox",
		"me@hey.com", mboxPath,
		"--source-type", "hey",
		"--label", "hey",
		"--no-resume",
		"--checkpoint-interval", "1",
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()), "import-mbox")

	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	var (
		sourceCount     int
		messageCount    int
		attachmentCount int
	)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sources WHERE source_type = 'hey' AND identifier = 'me@hey.com'`).Scan(&sourceCount), "count sources")
	require.Equal(1, sourceCount, "sourceCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages")
	require.Equal(2, messageCount, "messageCount")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentCount), "count attachments")
	require.Equal(1, attachmentCount, "attachmentCount")

	var storagePath string
	require.NoError(st.DB().QueryRow(`SELECT storage_path FROM attachments LIMIT 1`).Scan(&storagePath), "select storage_path")
	require.NotEmpty(storagePath, "storage_path empty")
	_, err = os.Stat(filepath.Join(tmp, "attachments", filepath.FromSlash(storagePath)))
	require.NoError(err, "attachment file missing")
}

func TestImportMboxCmd_AttachmentFailureIsBestEffort(t *testing.T) {
	tmp := t.TempDir()

	// Save/restore global state for cmd package.
	prevCfg := cfg
	prevLogger := logger
	prevSourceType := importMboxSourceType
	prevLabel := importMboxLabels
	prevNoResume := importMboxNoResume
	prevCheckpointInterval := importMboxCheckpointInterval
	prevNoAttachments := importMboxNoAttachments
	prevCfgFile := cfgFile
	prevHomeDir := homeDir
	prevVerbose := verbose
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		cfg = prevCfg
		logger = prevLogger
		importMboxSourceType = prevSourceType
		importMboxLabels = prevLabel
		importMboxNoResume = prevNoResume
		importMboxCheckpointInterval = prevCheckpointInterval
		importMboxNoAttachments = prevNoAttachments
		cfgFile = prevCfgFile
		homeDir = prevHomeDir
		verbose = prevVerbose
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	})

	// Force attachment storage errors by making the attachments path a file.
	requirepkg.NoError(t, os.WriteFile(filepath.Join(tmp, "attachments"), []byte("not a dir"), 0600), "write attachments sentinel")

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		WithAttachment("a.txt", "text/plain", []byte("hello")).
		Bytes()

	var mbox strings.Builder
	mbox.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mbox.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mbox.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	requirepkg.NoError(t, os.WriteFile(mboxPath, []byte(mbox.String()), 0600), "write mbox")

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-mbox",
		"me@hey.com", mboxPath,
		"--source-type", "hey",
		"--no-resume",
		"--checkpoint-interval", "1",
	})

	// Attachment storage failures are best-effort: the import
	// succeeds even though the attachment file can't be written.
	requirepkg.NoError(t, rootCmd.ExecuteContext(context.Background()), "expected success")
}

func TestImportMboxCmd_ReturnsCanceledWhenContextCanceled(t *testing.T) {
	tmp := t.TempDir()

	// Save/restore global state for cmd package.
	prevCfg := cfg
	prevLogger := logger
	prevRootCtx := rootCmd.Context()
	prevSourceType := importMboxSourceType
	prevLabel := importMboxLabels
	prevNoResume := importMboxNoResume
	prevCheckpointInterval := importMboxCheckpointInterval
	prevNoAttachments := importMboxNoAttachments
	prevImportCtx := importMboxCmd.Context()
	prevCfgFile := cfgFile
	prevHomeDir := homeDir
	prevVerbose := verbose
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		cfg = prevCfg
		logger = prevLogger
		rootCmd.SetContext(prevRootCtx)
		importMboxSourceType = prevSourceType
		importMboxLabels = prevLabel
		importMboxNoResume = prevNoResume
		importMboxCheckpointInterval = prevCheckpointInterval
		importMboxNoAttachments = prevNoAttachments
		importMboxCmd.SetContext(prevImportCtx)
		cfgFile = prevCfgFile
		homeDir = prevHomeDir
		verbose = prevVerbose
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	})

	raw := email.NewMessage().
		From("Alice <alice@example.com>").
		To("Bob <bob@example.com>").
		Subject("Hello").
		Date("Mon, 01 Jan 2024 12:00:00 +0000").
		Header("Message-ID", "<msg1@example.com>").
		Body("Hi Bob.\n").
		Bytes()

	var mbox strings.Builder
	mbox.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mbox.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mbox.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	requirepkg.NoError(t, os.WriteFile(mboxPath, []byte(mbox.String()), 0600), "write mbox")

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-mbox",
		"me@hey.com", mboxPath,
		"--source-type", "hey",
		"--no-resume",
		"--checkpoint-interval", "1",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Set the import command's context to the canceled context directly,
	// since Cobra child commands with their own context don't inherit
	// from the root's ExecuteContext.
	importMboxCmd.SetContext(ctx)

	err := rootCmd.ExecuteContext(ctx)
	requirepkg.Error(t, err, "expected error")
	requirepkg.ErrorIs(t, err, context.Canceled, "expected context.Canceled")
}

func TestImportMboxCmd_EndToEnd_ZipResumeAcrossFiles(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	// Save/restore global state for cmd package.
	prevCfg := cfg
	prevLogger := logger
	prevSourceType := importMboxSourceType
	prevLabel := importMboxLabels
	prevNoResume := importMboxNoResume
	prevCheckpointInterval := importMboxCheckpointInterval
	prevNoAttachments := importMboxNoAttachments
	prevCfgFile := cfgFile
	prevHomeDir := homeDir
	prevVerbose := verbose
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		cfg = prevCfg
		logger = prevLogger
		importMboxSourceType = prevSourceType
		importMboxLabels = prevLabel
		importMboxNoResume = prevNoResume
		importMboxCheckpointInterval = prevCheckpointInterval
		importMboxNoAttachments = prevNoAttachments
		cfgFile = prevCfgFile
		homeDir = prevHomeDir
		verbose = prevVerbose
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	})

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
		Header("In-Reply-To", "<msg1@example.com>").
		Header("References", "<msg1@example.com>").
		Body("Msg2.\n").
		Bytes()

	raw3 := email.NewMessage().
		From("Carol <carol@example.com>").
		To("Alice <alice@example.com>").
		Subject("Three").
		Date("Mon, 01 Jan 2024 14:00:00 +0000").
		Header("Message-ID", "<msg3@example.com>").
		Body("Msg3.\n").
		Bytes()

	var mbox1 strings.Builder
	mbox1.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mbox1.Write(raw1)
	if !strings.HasSuffix(string(raw1), "\n") {
		mbox1.WriteString("\n")
	}
	mbox1.WriteString("From bob@example.com Mon Jan 1 13:00:00 2024\n")
	mbox1.Write(raw2)
	if !strings.HasSuffix(string(raw2), "\n") {
		mbox1.WriteString("\n")
	}

	var mbox2 strings.Builder
	mbox2.WriteString("From carol@example.com Mon Jan 1 14:00:00 2024\n")
	mbox2.Write(raw3)
	if !strings.HasSuffix(string(raw3), "\n") {
		mbox2.WriteString("\n")
	}

	zipPath := filepath.Join(tmp, "export.zip")
	writeZipFile(t, zipPath, map[string]string{
		"a.mbox": mbox1.String(),
		"b.mbox": mbox2.String(),
	})

	mboxFirstOnlyPath := filepath.Join(tmp, "first-only.mbox")
	require.NoError(os.WriteFile(mboxFirstOnlyPath, []byte("From alice@example.com Mon Jan 1 12:00:00 2024\n"+string(raw1)), 0600), "write mbox")
	if !strings.HasSuffix(string(raw1), "\n") {
		require.NoError(os.WriteFile(mboxFirstOnlyPath, append([]byte("From alice@example.com Mon Jan 1 12:00:00 2024\n"), append(raw1, '\n')...), 0600), "write mbox (newline)")
	}

	// Pre-import the first message.
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")
	_, err = importer.ImportMbox(context.Background(), st, mboxFirstOnlyPath, importer.MboxImportOptions{
		SourceType:         "hey",
		Identifier:         "me@hey.com",
		NoResume:           true,
		CheckpointInterval: 1,
	})
	require.NoError(err, "pre-import")

	// Extract the zip so we can compute a checkpoint offset within the first extracted file.
	extracted, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	require.NoError(err, "resolveMboxExport")
	require.Len(extracted, 2, "len(extracted)")

	f, err := os.Open(extracted[0])
	require.NoError(err, "open extracted")
	r := mbox.NewReader(f)
	if _, err := r.Next(); err != nil {
		_ = f.Close()
		require.NoError(err, "read first message")
	}
	offset := r.NextFromOffset()
	_ = f.Close()

	src, err := st.GetOrCreateSource("hey", "me@hey.com")
	require.NoError(err, "get/create source")
	syncID, err := st.StartSync(src.ID, "import-mbox")
	require.NoError(err, "start sync")
	cpFile := extracted[0]
	linkPath := filepath.Join(tmp, "checkpoint-link.mbox")
	if err := os.Symlink(extracted[0], linkPath); err == nil {
		cpFile = linkPath
	}
	b, err := json.Marshal(mboxCheckpoint{File: cpFile, Offset: offset, Seq: 1})
	require.NoError(err, "marshal checkpoint")
	cp := &store.Checkpoint{
		PageToken:         string(b),
		MessagesProcessed: 1,
		MessagesAdded:     1,
	}
	require.NoError(st.UpdateSyncCheckpoint(syncID, cp), "update checkpoint")
	require.NoError(st.Close(), "close store")

	// Resume import from the zip export and ensure it continues into subsequent files.
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{
		"--home", tmp,
		"import-mbox",
		"me@hey.com", zipPath,
		"--source-type", "hey",
		"--checkpoint-interval", "1",
		"--no-attachments",
	})
	require.NoError(rootCmd.ExecuteContext(context.Background()), "import-mbox resume")

	st2, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st2.Close() })

	var messageCount int
	require.NoError(st2.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount), "count messages")
	require.Equal(3, messageCount, "messageCount")

	for _, subj := range []string{"One", "Two", "Three"} {
		var c int
		err := st2.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE subject = ?`, subj).Scan(&c)
		require.NoError(err, "count subject %q", subj)
		assertpkg.Equal(t, 1, c, "subject %q count", subj)
	}
}
