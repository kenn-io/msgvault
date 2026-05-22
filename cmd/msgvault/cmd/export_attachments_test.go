package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestExportAttachmentsCmd_Registration(t *testing.T) {
	// Verify the command is registered and has expected configuration
	cmd, _, err := rootCmd.Find([]string{"export-attachments"})
	if err != nil {
		t.Fatalf("export-attachments command not found: %v", err)
	}
	if cmd.Use != "export-attachments <message-id>" {
		t.Errorf("Use = %q, want %q", cmd.Use, "export-attachments <message-id>")
	}

	// Verify -o flag exists
	f := cmd.Flags().Lookup("output")
	if f == nil {
		t.Fatal("expected --output flag")
	}
	if f.Shorthand != "o" {
		t.Errorf("output shorthand = %q, want %q", f.Shorthand, "o")
	}
}

// setupExportAttachmentsTest creates a temp directory with a SQLite database
// containing a message with attachments and corresponding content-addressed
// files on disk. Returns the data dir and the message ID.
func setupExportAttachmentsTest(t *testing.T) (dataDir string, msgID int64) {
	t.Helper()
	dataDir = t.TempDir()

	dbPath := filepath.Join(dataDir, "msgvault.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatal(err)
	}

	db := s.DB()

	// Insert source, conversation, message
	if _, err := db.Exec("INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'test@gmail.com')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (1, 1, 'conv1', 'email_thread')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, subject, sent_at, has_attachments)
		VALUES (1, 1, 'gmail_abc123', 1, 'email', 'Test Message', '2024-06-01 10:00:00', 1)`); err != nil {
		t.Fatal(err)
	}

	// Create attachment files on disk and insert metadata
	attDir := filepath.Join(dataDir, "attachments")
	createTestAttachment(t, db, attDir, 1, 1, "report.pdf", []byte("PDF content here"))
	createTestAttachment(t, db, attDir, 2, 1, "photo.jpg", []byte("JPEG image data"))

	_ = s.Close()
	return dataDir, 1
}

// createTestAttachment creates a content-addressed file and inserts the
// attachment metadata into the database.
func createTestAttachment(t *testing.T, db *sql.DB, attDir string, attID, msgID int64, filename string, content []byte) {
	t.Helper()
	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	// Write content-addressed file
	dir := filepath.Join(attDir, hash[:2])
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hash), content, 0644); err != nil {
		t.Fatal(err)
	}

	// Insert attachment record
	storagePath := hash[:2] + "/" + hash
	_, err := db.Exec(
		`INSERT INTO attachments (id, message_id, filename, mime_type, size, content_hash, storage_path)
		 VALUES (?, ?, ?, 'application/octet-stream', ?, ?, ?)`,
		attID, msgID, filename, len(content), hash, storagePath,
	)
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
}

func TestExportAttachments_FullFlow(t *testing.T) {
	dataDir, _ := setupExportAttachmentsTest(t)

	// Set global cfg to point to our test data
	oldCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	defer func() { cfg = oldCfg }()

	outputDir := t.TempDir()
	exportAttachmentsOutput = outputDir
	defer func() { exportAttachmentsOutput = "" }()

	c := exportAttachmentsCmd
	c.SetContext(context.Background())
	err := runExportAttachments(c, []string{"1"})
	if err != nil {
		t.Fatalf("runExportAttachments: %v", err)
	}

	// Verify both files were exported
	entries, _ := os.ReadDir(outputDir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files, got %d", len(entries))
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["report.pdf"] {
		t.Error("expected report.pdf in output")
	}
	if !names["photo.jpg"] {
		t.Error("expected photo.jpg in output")
	}
}

func TestExportAttachments_GmailIDFallback(t *testing.T) {
	dataDir, _ := setupExportAttachmentsTest(t)

	oldCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	defer func() { cfg = oldCfg }()

	outputDir := t.TempDir()
	exportAttachmentsOutput = outputDir
	defer func() { exportAttachmentsOutput = "" }()

	// Use Gmail source ID instead of numeric ID
	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"gmail_abc123"})
	if err != nil {
		t.Fatalf("runExportAttachments with Gmail ID: %v", err)
	}

	entries, _ := os.ReadDir(outputDir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files from Gmail ID lookup, got %d", len(entries))
	}
}

func TestExportAttachments_MessageNotFound(t *testing.T) {
	dataDir, _ := setupExportAttachmentsTest(t)

	oldCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	defer func() { cfg = oldCfg }()

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"99999"})
	if err == nil {
		t.Fatal("expected error for nonexistent message")
	}
	if !contains(err.Error(), "message not found") {
		t.Errorf("error = %q, want containing 'message not found'", err)
	}
}

func TestExportAttachments_OutputDirValidation(t *testing.T) {
	dataDir, _ := setupExportAttachmentsTest(t)

	oldCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	defer func() { cfg = oldCfg }()

	// Point to a non-existent directory
	exportAttachmentsOutput = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { exportAttachmentsOutput = "" }()

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"1"})
	if err == nil {
		t.Fatal("expected error for non-existent output directory")
	}
	if !contains(err.Error(), "output directory") {
		t.Errorf("error = %q, want containing 'output directory'", err)
	}
}

func TestExportAttachments_NotADirectory(t *testing.T) {
	dataDir, _ := setupExportAttachmentsTest(t)

	oldCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	defer func() { cfg = oldCfg }()

	// Point to a file, not a directory
	tmpFile := filepath.Join(t.TempDir(), "afile.txt")
	if err := os.WriteFile(tmpFile, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	exportAttachmentsOutput = tmpFile
	defer func() { exportAttachmentsOutput = "" }()

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"1"})
	if err == nil {
		t.Fatal("expected error for file as output dir")
	}
	if !contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want containing 'not a directory'", err)
	}
}
