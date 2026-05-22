package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/importer/mboxzip"
	"go.kenn.io/msgvault/internal/mbox"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportMboxCmd_EndToEnd_MboxFile(t *testing.T) {
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
	if err := os.WriteFile(mboxPath, []byte(mbox.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

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
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("import-mbox: %v", err)
	}

	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var (
		sourceCount     int
		messageCount    int
		attachmentCount int
	)
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sources WHERE source_type = 'hey' AND identifier = 'me@hey.com'`).Scan(&sourceCount); err != nil {
		t.Fatalf("count sources: %v", err)
	}
	if sourceCount != 1 {
		t.Fatalf("sourceCount = %d, want 1", sourceCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 2 {
		t.Fatalf("messageCount = %d, want 2", messageCount)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachmentCount); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachmentCount != 1 {
		t.Fatalf("attachmentCount = %d, want 1", attachmentCount)
	}

	var storagePath string
	if err := st.DB().QueryRow(`SELECT storage_path FROM attachments LIMIT 1`).Scan(&storagePath); err != nil {
		t.Fatalf("select storage_path: %v", err)
	}
	if storagePath == "" {
		t.Fatalf("storage_path empty")
	}
	if _, err := os.Stat(filepath.Join(tmp, "attachments", filepath.FromSlash(storagePath))); err != nil {
		t.Fatalf("attachment file missing: %v", err)
	}
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
	if err := os.WriteFile(filepath.Join(tmp, "attachments"), []byte("not a dir"), 0600); err != nil {
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

	var mbox strings.Builder
	mbox.WriteString("From alice@example.com Mon Jan 1 12:00:00 2024\n")
	mbox.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		mbox.WriteString("\n")
	}

	mboxPath := filepath.Join(tmp, "export.mbox")
	if err := os.WriteFile(mboxPath, []byte(mbox.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

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
	err := rootCmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
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
	if err := os.WriteFile(mboxPath, []byte(mbox.String()), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}

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
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestImportMboxCmd_EndToEnd_ZipResumeAcrossFiles(t *testing.T) {
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
	if err := os.WriteFile(mboxFirstOnlyPath, []byte("From alice@example.com Mon Jan 1 12:00:00 2024\n"+string(raw1)), 0600); err != nil {
		t.Fatalf("write mbox: %v", err)
	}
	if !strings.HasSuffix(string(raw1), "\n") {
		if err := os.WriteFile(mboxFirstOnlyPath, append([]byte("From alice@example.com Mon Jan 1 12:00:00 2024\n"), append(raw1, '\n')...), 0600); err != nil {
			t.Fatalf("write mbox (newline): %v", err)
		}
	}

	// Pre-import the first message.
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if _, err := importer.ImportMbox(context.Background(), st, mboxFirstOnlyPath, importer.MboxImportOptions{
		SourceType:         "hey",
		Identifier:         "me@hey.com",
		NoResume:           true,
		CheckpointInterval: 1,
	}); err != nil {
		t.Fatalf("pre-import: %v", err)
	}

	// Extract the zip so we can compute a checkpoint offset within the first extracted file.
	extracted, err := mboxzip.ResolveMboxExport(zipPath, tmp, nil)
	if err != nil {
		t.Fatalf("resolveMboxExport: %v", err)
	}
	if len(extracted) != 2 {
		t.Fatalf("len(extracted) = %d, want 2", len(extracted))
	}

	f, err := os.Open(extracted[0])
	if err != nil {
		t.Fatalf("open extracted: %v", err)
	}
	r := mbox.NewReader(f)
	if _, err := r.Next(); err != nil {
		_ = f.Close()
		t.Fatalf("read first message: %v", err)
	}
	offset := r.NextFromOffset()
	_ = f.Close()

	src, err := st.GetOrCreateSource("hey", "me@hey.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	syncID, err := st.StartSync(src.ID, "import-mbox")
	if err != nil {
		t.Fatalf("start sync: %v", err)
	}
	cpFile := extracted[0]
	linkPath := filepath.Join(tmp, "checkpoint-link.mbox")
	if err := os.Symlink(extracted[0], linkPath); err == nil {
		cpFile = linkPath
	}
	b, err := json.Marshal(mboxCheckpoint{File: cpFile, Offset: offset, Seq: 1})
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	cp := &store.Checkpoint{
		PageToken:         string(b),
		MessagesProcessed: 1,
		MessagesAdded:     1,
	}
	if err := st.UpdateSyncCheckpoint(syncID, cp); err != nil {
		t.Fatalf("update checkpoint: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

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
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("import-mbox resume: %v", err)
	}

	st2, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	var messageCount int
	if err := st2.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 3 {
		t.Fatalf("messageCount = %d, want 3", messageCount)
	}

	for _, subj := range []string{"One", "Two", "Three"} {
		var c int
		if err := st2.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE subject = ?`, subj).Scan(&c); err != nil {
			t.Fatalf("count subject %q: %v", subj, err)
		}
		if c != 1 {
			t.Fatalf("subject %q count = %d, want 1", subj, c)
		}
	}
}
