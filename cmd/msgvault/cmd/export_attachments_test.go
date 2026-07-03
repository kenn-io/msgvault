package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestExportAttachmentsCmd_Registration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Verify the command is registered and has expected configuration
	cmd, _, err := rootCmd.Find([]string{"export-attachments"})
	require.NoError(err, "export-attachments command not found")
	assert.Equal("export-attachments <message-id>", cmd.Use, "Use")

	// Verify -o flag exists
	f := cmd.Flags().Lookup("output")
	require.NotNil(f, "expected --output flag")
	assert.Equal("o", f.Shorthand, "output shorthand")
}

func setupExportAttachmentsHTTPTest(t *testing.T) ([]byte, []byte, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	dataDir := t.TempDir()
	reportData := []byte("PDF content here")
	photoData := []byte("JPEG image data")
	reportHash := fmt.Sprintf("%x", sha256.Sum256(reportData))
	photoHash := fmt.Sprintf("%x", sha256.Sum256(photoData))
	server, messageRequests, attachmentRequests := exportAttachmentsHTTPDaemon(
		t,
		reportHash,
		reportData,
		photoHash,
		photoData,
	)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)
	configureExportAttachmentsDaemonTest(t, dataDir)
	return reportData, photoData, messageRequests, attachmentRequests
}

func configureExportAttachmentsDaemonTest(t *testing.T, dataDir string) {
	t.Helper()
	oldCfg := cfg
	oldUseLocal := useLocal
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	t.Cleanup(func() {
		cfg = oldCfg
		useLocal = oldUseLocal
	})
}

func TestResolveExportAttachmentsOutputDir_CreatesMissingDir(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	oldOutput := exportAttachmentsOutput
	t.Cleanup(func() { exportAttachmentsOutput = oldOutput })

	target := filepath.Join(t.TempDir(), "nested", "attachments")
	exportAttachmentsOutput = target

	got, err := resolveExportAttachmentsOutputDir()
	require.NoError(err, "resolveExportAttachmentsOutputDir")

	info, statErr := os.Stat(target)
	require.NoError(statErr, "stat created dir")
	assert.True(info.IsDir(), "created path is a directory")

	absTarget, err := filepath.Abs(target)
	require.NoError(err, "abs target")
	assert.Equal(absTarget, got, "returned absolute output dir")
}

func TestResolveExportAttachmentsOutputDir_RejectsFilePath(t *testing.T) {
	oldOutput := exportAttachmentsOutput
	t.Cleanup(func() { exportAttachmentsOutput = oldOutput })

	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o600), "write file")
	exportAttachmentsOutput = filePath

	_, err := resolveExportAttachmentsOutputDir()
	require.Error(t, err, "expected error for file output path")
	assert.Contains(t, err.Error(), "not a directory", "error text")
}

func TestExportAttachments_FullFlow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setupExportAttachmentsHTTPTest(t)

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

func TestExportAttachmentsUsesLocalDaemonHTTPAndPreservesDirectoryOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	outputDir := t.TempDir()
	reportData, photoData, messageRequests, attachmentRequests := setupExportAttachmentsHTTPTest(t)

	oldOutput := exportAttachmentsOutput
	defer func() {
		exportAttachmentsOutput = oldOutput
	}()
	exportAttachmentsOutput = outputDir

	doneErr := captureStderr(t)
	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())

	err := runExportAttachments(cmd, []string{"gmail_abc123"})
	stderr := doneErr()
	require.NoError(err, "runExportAttachments")

	reportOut, err := os.ReadFile(filepath.Join(outputDir, "report.pdf"))
	require.NoError(err, "read report")
	photoOut, err := os.ReadFile(filepath.Join(outputDir, "photo.jpg"))
	require.NoError(err, "read photo")
	assert.Equal(reportData, reportOut, "report data")
	assert.Equal(photoData, photoOut, "photo data")
	assert.Equal(1, int(messageRequests.Load()), "message endpoint calls")
	assert.Equal(2, int(attachmentRequests.Load()), "attachment endpoint calls")
	assert.Contains(stderr, "  report.pdf (", "report stderr")
	assert.Contains(stderr, "  photo.jpg (", "photo stderr")
	assert.Contains(stderr, "Exported 2 attachment(s)", "summary")
	assert.Contains(stderr, "to "+outputDir, "summary dir")
}

func TestExportAttachments_GmailIDFallback(t *testing.T) {
	setupExportAttachmentsHTTPTest(t)

	outputDir := t.TempDir()
	exportAttachmentsOutput = outputDir
	defer func() { exportAttachmentsOutput = "" }()

	// Use Gmail source ID instead of numeric ID
	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	require.NoError(t, runExportAttachments(cmd, []string{"gmail_abc123"}), "runExportAttachments with Gmail ID")

	entries, _ := os.ReadDir(outputDir)
	require.Len(t, entries, 2, "expected 2 files from Gmail ID lookup")
}

func TestExportAttachments_MessageNotFound(t *testing.T) {
	setupExportAttachmentsHTTPTest(t)

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"99999"})
	require.Error(t, err, "expected error for nonexistent message")
	assert.ErrorContains(t, err, "message not found")
}

func TestExportAttachments_OutputDirAutoCreated(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setupExportAttachmentsHTTPTest(t)

	// Point to a non-existent nested directory; it should be created like the
	// sibling exporters create the file/path they are asked to write to.
	outputDir := filepath.Join(t.TempDir(), "does-not-exist", "nested")
	exportAttachmentsOutput = outputDir
	defer func() { exportAttachmentsOutput = "" }()

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	require.NoError(runExportAttachments(cmd, []string{"1"}), "runExportAttachments")

	entries, err := os.ReadDir(outputDir)
	require.NoError(err, "read created output dir")
	assert.Len(entries, 2, "expected 2 exported files")
}

func TestExportAttachments_NotADirectory(t *testing.T) {
	setupExportAttachmentsHTTPTest(t)

	// Point to a file, not a directory
	tmpFile := filepath.Join(t.TempDir(), "afile.txt")
	require.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0644))
	exportAttachmentsOutput = tmpFile
	defer func() { exportAttachmentsOutput = "" }()

	cmd := exportAttachmentsCmd
	cmd.SetContext(context.Background())
	err := runExportAttachments(cmd, []string{"1"})
	require.Error(t, err, "expected error for file as output dir")
	assert.ErrorContains(t, err, "not a directory")
}

func exportAttachmentsHTTPDaemon(
	t *testing.T,
	reportHash string,
	reportData []byte,
	photoHash string,
	photoData []byte,
) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	messageRequests := &atomic.Int32{}
	attachmentRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id != "1" && id != "gmail_abc123" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		messageRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"id": 1,
			"source_message_id": "gmail_abc123",
			"conversation_id": 1,
			"subject": "Test Message",
			"sent_at": "2024-06-01T10:00:00Z",
			"has_attachments": true,
			"attachments": [
				{"id": 1, "filename": "report.pdf", "mime_type": "application/pdf", "size": %d, "content_hash": %q},
				{"id": 2, "filename": "photo.jpg", "mime_type": "image/jpeg", "size": %d, "content_hash": %q}
			]
		}`, len(reportData), reportHash, len(photoData), photoHash)
	})
	mux.HandleFunc("/api/v1/cli/attachment", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		attachmentRequests.Add(1)
		switch r.URL.Query().Get("content_hash") {
		case reportHash:
			_, _ = w.Write(reportData)
		case photoHash:
			_, _ = w.Write(photoData)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, messageRequests, attachmentRequests
}
