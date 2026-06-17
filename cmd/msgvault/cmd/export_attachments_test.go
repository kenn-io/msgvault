package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestExportAttachmentsCmd_Registration(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Verify the command is registered and has expected configuration
	cmd, _, err := rootCmd.Find([]string{"export-attachments"})
	require.NoError(err, "export-attachments command not found")
	assert.Equal("export-attachments <message-id>", cmd.Use, "Use")

	// Verify -o flag exists
	f := cmd.Flags().Lookup("output")
	require.NotNil(f, "expected --output flag")
	assert.Equal("o", f.Shorthand, "output shorthand")
}

// setupExportAttachmentsTest creates a temp directory with a SQLite database
// containing a message with attachments and corresponding content-addressed
// files on disk. Returns the data dir.
func setupExportAttachmentsTest(t *testing.T) (dataDir string) {
	t.Helper()
	dataDir = t.TempDir()

	dbPath := filepath.Join(dataDir, "msgvault.db")
	s, err := store.Open(dbPath)
	requirepkg.NoError(t, err)
	requirepkg.NoError(t, s.InitSchema())

	db := s.DB()

	// Insert source, conversation, message
	_, err = db.Exec("INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'test@gmail.com')")
	requirepkg.NoError(t, err)
	_, err = db.Exec("INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (1, 1, 'conv1', 'email_thread')")
	requirepkg.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, subject, sent_at, has_attachments)
		VALUES (1, 1, 'gmail_abc123', 1, 'email', 'Test Message', '2024-06-01 10:00:00', 1)`)
	requirepkg.NoError(t, err)

	// Create attachment files on disk and insert metadata
	attDir := filepath.Join(dataDir, "attachments")
	createTestAttachment(t, db, attDir, 1, 1, "report.pdf", []byte("PDF content here"))
	createTestAttachment(t, db, attDir, 2, 1, "photo.jpg", []byte("JPEG image data"))

	_ = s.Close()
	return dataDir
}

// createTestAttachment creates a content-addressed file and inserts the
// attachment metadata into the database.
func createTestAttachment(t *testing.T, db *sql.DB, attDir string, attID, msgID int64, filename string, content []byte) {
	t.Helper()
	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	// Write content-addressed file
	dir := filepath.Join(attDir, hash[:2])
	requirepkg.NoError(t, os.MkdirAll(dir, 0755))
	requirepkg.NoError(t, os.WriteFile(filepath.Join(dir, hash), content, 0644))

	// Insert attachment record
	storagePath := hash[:2] + "/" + hash
	_, err := db.Exec(
		`INSERT INTO attachments (id, message_id, filename, mime_type, size, content_hash, storage_path)
		 VALUES (?, ?, ?, 'application/octet-stream', ?, ?, ?)`,
		attID, msgID, filename, len(content), hash, storagePath,
	)
	requirepkg.NoError(t, err, "insert attachment")
}

func TestExportAttachments_FullFlow(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dataDir := setupExportAttachmentsTest(t)

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
	require.NoError(runExportAttachments(c, []string{"1"}), "runExportAttachments")

	// Verify both files were exported
	entries, _ := os.ReadDir(outputDir)
	require.Len(entries, 2, "expected 2 files")

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	assert.True(names["report.pdf"], "expected report.pdf in output")
	assert.True(names["photo.jpg"], "expected photo.jpg in output")
}

func TestExportAttachments_GmailIDFallback(t *testing.T) {
	dataDir := setupExportAttachmentsTest(t)

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
	requirepkg.NoError(t, runExportAttachments(cmd, []string{"gmail_abc123"}), "runExportAttachments with Gmail ID")

	entries, _ := os.ReadDir(outputDir)
	requirepkg.Len(t, entries, 2, "expected 2 files from Gmail ID lookup")
}

func TestExportAttachments_MessageNotFound(t *testing.T) {
	dataDir := setupExportAttachmentsTest(t)

	oldCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	defer func() { cfg = oldCfg }()

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"99999"})
	requirepkg.Error(t, err, "expected error for nonexistent message")
	assertpkg.ErrorContains(t, err, "message not found")
}

func TestExportAttachments_OutputDirValidation(t *testing.T) {
	dataDir := setupExportAttachmentsTest(t)

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
	requirepkg.Error(t, err, "expected error for non-existent output directory")
	assertpkg.ErrorContains(t, err, "output directory")
}

func TestExportAttachments_NotADirectory(t *testing.T) {
	dataDir := setupExportAttachmentsTest(t)

	oldCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	defer func() { cfg = oldCfg }()

	// Point to a file, not a directory
	tmpFile := filepath.Join(t.TempDir(), "afile.txt")
	requirepkg.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0644))
	exportAttachmentsOutput = tmpFile
	defer func() { exportAttachmentsOutput = "" }()

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"1"})
	requirepkg.Error(t, err, "expected error for file as output dir")
	assertpkg.ErrorContains(t, err, "not a directory")
}
