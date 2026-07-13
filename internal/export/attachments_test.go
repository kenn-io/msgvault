package export

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

type closeErrorReader struct {
	io.Reader

	err error
}

func (r *closeErrorReader) Close() error { return r.err }

// createAttachmentFile creates a file in the content-addressed storage layout
// (root/<hash[:2]>/<hash>) and returns the SHA-256 hex hash of the content.
func createAttachmentFile(t *testing.T, root string, content []byte) string {
	t.Helper()
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	dir := filepath.Join(root, hash[:2])
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, hash), content, 0o644))
	return hash
}

func TestFormatExportResult_WriteErrorWithCount(t *testing.T) {
	assert := assert.New(t)
	// Test that WriteError flag causes failure message even when Count > 0
	stats := ExportStats{
		Count:      5,
		Size:       1024,
		WriteError: true,
		Errors:     []string{"zip finalization error: disk full"},
		ZipPath:    "", // Empty because zip was removed
	}

	result := FormatExportResult(stats)

	// Should report failure, not success
	assert.Contains(result, "Export failed due to write errors", "expected failure message")
	assert.Contains(result, "Zip file removed", "expected 'Zip file removed'")
	assert.NotContains(result, "Exported 5 attachment",
		"should not report success count when WriteError is true")
	assert.NotContains(result, "Saved to:",
		"should not show 'Saved to:' when WriteError is true")
}

func TestPathTraversalInContentHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// This test verifies that malicious content hashes with path traversal
	// sequences are rejected and cannot be used to read arbitrary files.

	// Create a "secret" file outside the attachments directory
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "secret.txt")
	secretContent := []byte("TOP SECRET DATA")
	require.NoError(os.WriteFile(secretFile, secretContent, 0o644))

	// Create attachments directory (subdirectory to enable traversal)
	attachDir := filepath.Join(t.TempDir(), "attachments")
	require.NoError(os.MkdirAll(attachDir, 0o755))

	// Craft a malicious content hash that would traverse to the secret file
	// The path construction is: root/<hash[:2]>/<hash>
	// We need to escape from root/../ to reach secretDir
	// Since hash[:2] is used as subdir, we use ".." as the prefix
	maliciousHash := "../" + secretFile

	zipPath := filepath.Join(t.TempDir(), "test.zip")
	inputs := []query.AttachmentInfo{
		{Filename: "stolen.txt", ContentHash: maliciousHash},
	}

	stats := Attachments(zipPath, attachDir, inputs)

	// The export should fail - path traversal should be detected and rejected
	assert.Zero(stats.Count, "path traversal attack succeeded: exported %d files", stats.Count)

	// Should report an error about the invalid content hash
	found := false
	for _, errMsg := range stats.Errors {
		if strings.Contains(errMsg, "invalid content hash") {
			found = true
			break
		}
	}
	assert.True(found, "expected 'invalid content hash' error, got errors: %v", stats.Errors)
}

func TestContentHashValidation(t *testing.T) {
	tests := []struct {
		name    string
		hash    string
		wantErr bool
	}{
		{"valid sha256 lowercase", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", false},
		{"valid sha256 uppercase", "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2", false},
		{"valid sha256 mixed case", "A1b2C3d4E5f6A1b2C3d4E5f6A1b2C3d4E5f6A1b2C3d4E5f6A1b2C3d4E5f6A1b2", false},
		{"path traversal ../", "../../../etc/passwd", true},
		{"path traversal ..\\", "..\\..\\windows\\system32", true},
		{"contains slash", "abc/def", true},
		{"contains backslash", "abc\\def", true},
		{"too short", "abc123", true},
		{"too long", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2extra", true},
		{"non-hex characters", "g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true},
		{"empty string", "", true},
		{"null bytes", "a1b2c3d4\x00e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateContentHash(tt.hash)
			if tt.wantErr {
				assert.Error(t, err, "ValidateContentHash(%q)", tt.hash)
			} else {
				assert.NoError(t, err, "ValidateContentHash(%q)", tt.hash)
			}
		})
	}
}

func TestAttachmentsToDir(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, attachDir string) []query.AttachmentInfo
		wantFiles  int
		wantErrors int
		wantNames  []string // basenames of expected output files
	}{
		{
			name: "single file exported",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				hash := createAttachmentFile(t, attachDir, []byte("hello world"))
				return []query.AttachmentInfo{{Filename: "greeting.txt", ContentHash: hash}}
			},
			wantFiles: 1,
			wantNames: []string{"greeting.txt"},
		},
		{
			name: "multiple files exported",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				h1 := createAttachmentFile(t, attachDir, []byte("file one"))
				h2 := createAttachmentFile(t, attachDir, []byte("file two"))
				return []query.AttachmentInfo{
					{Filename: "one.txt", ContentHash: h1},
					{Filename: "two.txt", ContentHash: h2},
				}
			},
			wantFiles: 2,
			wantNames: []string{"one.txt", "two.txt"},
		},
		{
			name: "invalid hash is skipped",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return []query.AttachmentInfo{{Filename: "bad.txt", ContentHash: "short"}}
			},
			wantErrors: 1,
		},
		{
			name: "url backed attachment is reported as link",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return []query.AttachmentInfo{{Filename: "deck.pptx", URL: "https://sp/deck.pptx"}}
			},
			wantErrors: 1,
		},
		{
			name: "missing content file is reported",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return []query.AttachmentInfo{{
					Filename:    "gone.txt",
					ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				}}
			},
			wantErrors: 1,
		},
		{
			name: "mix of valid and invalid",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				hash := createAttachmentFile(t, attachDir, []byte("good"))
				return []query.AttachmentInfo{
					{Filename: "bad.txt", ContentHash: ""},
					{Filename: "good.txt", ContentHash: hash},
				}
			},
			wantFiles:  1,
			wantErrors: 1,
			wantNames:  []string{"good.txt"},
		},
		{
			name: "duplicate filenames get deduped within batch",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				h1 := createAttachmentFile(t, attachDir, []byte("content A"))
				h2 := createAttachmentFile(t, attachDir, []byte("content B"))
				return []query.AttachmentInfo{
					{Filename: "same.txt", ContentHash: h1},
					{Filename: "same.txt", ContentHash: h2},
				}
			},
			wantFiles: 2,
			wantNames: []string{"same.txt", "same_2.txt"},
		},
		{
			name: "empty filename falls back to content hash",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				hash := createAttachmentFile(t, attachDir, []byte("no name"))
				return []query.AttachmentInfo{{Filename: "", ContentHash: hash}}
			},
			wantFiles: 1,
		},
		{
			name: "nil attachments",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			attachDir := t.TempDir()
			outputDir := t.TempDir()

			inputs := tt.setup(t, attachDir)
			result := AttachmentsToDir(outputDir, attachDir, inputs)

			require.Len(result.Files, tt.wantFiles, "files; errors: %v", result.Errors)
			require.Len(result.Errors, tt.wantErrors, "errors: %v", result.Errors)

			// Verify expected filenames
			for i, wantName := range tt.wantNames {
				if i >= len(result.Files) {
					break
				}
				gotName := filepath.Base(result.Files[i].Path)
				assert.Equal(wantName, gotName, "file[%d] name", i)
			}

			// Verify all exported files exist on disk and have correct content size
			for _, f := range result.Files {
				info, err := os.Stat(f.Path)
				require.NoError(err, "exported file %s does not exist", f.Path)
				assert.Equal(f.Size, info.Size(), "file %s size", f.Path)
			}
		})
	}
}

func TestAttachmentsToDirWithOpener(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := []byte("opener-served content")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	outDir := t.TempDir()
	atts := []query.AttachmentInfo{{Filename: "doc.txt", ContentHash: hash}}
	res := AttachmentsToDirWithOpener(outDir, atts, func(contentHash string) (io.ReadCloser, error) {
		require.Equal(hash, contentHash)
		return io.NopCloser(bytes.NewReader(content)), nil
	})

	require.Empty(res.Errors)
	require.Len(res.Files, 1)
	got, err := os.ReadFile(res.Files[0].Path)
	require.NoError(err)
	assert.Equal(content, got)
}

func TestAttachmentsToDirWithOpenerRejectsCloseVerificationError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := []byte("streamed content with terminal verification")
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	verificationErr := errors.New("terminal verification failed")
	outDir := t.TempDir()

	result := AttachmentsToDirWithOpener(outDir,
		[]query.AttachmentInfo{{Filename: "doc.txt", ContentHash: hash}},
		func(string) (io.ReadCloser, error) {
			return &closeErrorReader{Reader: bytes.NewReader(content), err: verificationErr}, nil
		})

	require.Empty(result.Files)
	require.Len(result.Errors, 1)
	assert.Contains(result.Errors[0], verificationErr.Error())
	entries, err := os.ReadDir(outDir)
	require.NoError(err)
	assert.Empty(entries, "a stream that fails terminal verification must not leave exported content")
}

func TestAttachmentsToDir_FilePermissions(t *testing.T) {
	attachDir := t.TempDir()
	outputDir := t.TempDir()

	hash := createAttachmentFile(t, attachDir, []byte("secret content"))
	inputs := []query.AttachmentInfo{{Filename: "doc.pdf", ContentHash: hash}}

	result := AttachmentsToDir(outputDir, attachDir, inputs)
	require.Len(t, result.Files, 1, "expected 1 file")

	// Windows does not support Unix permissions.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(result.Files[0].Path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "file permissions")
	}
}

func TestAttachmentsToDir_DiskConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Pre-existing file on disk should trigger _1 suffix
	attachDir := t.TempDir()
	outputDir := t.TempDir()

	// Create a pre-existing file
	require.NoError(os.WriteFile(filepath.Join(outputDir, "report.pdf"), []byte("old"), 0644))

	hash := createAttachmentFile(t, attachDir, []byte("new content"))
	inputs := []query.AttachmentInfo{{Filename: "report.pdf", ContentHash: hash}}

	result := AttachmentsToDir(outputDir, attachDir, inputs)
	require.Len(result.Files, 1, "expected 1 file; errors: %v", result.Errors)

	gotName := filepath.Base(result.Files[0].Path)
	assert.Equal("report_1.pdf", gotName)

	// Verify original file is untouched
	orig, _ := os.ReadFile(filepath.Join(outputDir, "report.pdf"))
	assert.Equal("old", string(orig), "original file was overwritten")
}

func TestCreateExclusiveFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("new file", func(t *testing.T) {
		p := filepath.Join(dir, "new.txt")
		f, path, err := CreateExclusiveFile(p, 0600)
		require.NoError(t, err)
		_ = f.Close()
		assert.Equal(t, p, path, "path")
		// Windows does not support Unix permissions.
		if runtime.GOOS != "windows" {
			info, _ := os.Stat(path)
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "permissions")
		}
	})

	t.Run("conflict appends suffix", func(t *testing.T) {
		p := filepath.Join(dir, "existing.txt")
		require.NoError(t, os.WriteFile(p, []byte("x"), 0644))

		f, path, err := CreateExclusiveFile(p, 0600)
		require.NoError(t, err)
		_ = f.Close()
		assert.Equal(t, "existing_1.txt", filepath.Base(path), "path")
	})

	t.Run("multiple conflicts", func(t *testing.T) {
		require := require.New(t)
		p := filepath.Join(dir, "multi.txt")
		require.NoError(os.WriteFile(p, []byte("x"), 0644))
		require.NoError(os.WriteFile(filepath.Join(dir, "multi_1.txt"), []byte("x"), 0644))

		f, path, err := CreateExclusiveFile(p, 0600)
		require.NoError(err)
		_ = f.Close()
		assert.Equal(t, "multi_2.txt", filepath.Base(path), "path")
	})

	t.Run("no extension", func(t *testing.T) {
		p := filepath.Join(dir, "noext")
		require.NoError(t, os.WriteFile(p, []byte("x"), 0644))

		f, path, err := CreateExclusiveFile(p, 0644)
		require.NoError(t, err)
		_ = f.Close()
		assert.Equal(t, "noext_1", filepath.Base(path), "path")
	})
}

func TestAttachments(t *testing.T) {
	tests := []struct {
		name           string
		setup          func(t *testing.T, attachDir string) []query.AttachmentInfo
		wantCount      int
		wantSubstrings []string
		wantFiles      []string // files expected in the zip
	}{
		{
			name: "valid file is exported",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				hash := createAttachmentFile(t, attachDir, []byte("hello world"))
				return []query.AttachmentInfo{{Filename: "greeting.txt", ContentHash: hash}}
			},
			wantCount:      1,
			wantSubstrings: []string{"Exported 1 attachment(s)"},
			wantFiles:      []string{"greeting.txt"},
		},
		{
			name: "multiple valid files exported",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				h1 := createAttachmentFile(t, attachDir, []byte("file one"))
				h2 := createAttachmentFile(t, attachDir, []byte("file two"))
				return []query.AttachmentInfo{
					{Filename: "one.txt", ContentHash: h1},
					{Filename: "two.txt", ContentHash: h2},
				}
			},
			wantCount:      2,
			wantSubstrings: []string{"Exported 2 attachment(s)"},
			wantFiles:      []string{"one.txt", "two.txt"},
		},
		{
			name: "empty content hash is skipped",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return []query.AttachmentInfo{{Filename: "file.txt", ContentHash: ""}}
			},
			wantSubstrings: []string{"file.txt: invalid content hash"},
		},
		{
			name: "url backed attachment reports link",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return []query.AttachmentInfo{{Filename: "deck.pptx", URL: "https://sp/deck.pptx"}}
			},
			wantSubstrings: []string{"deck.pptx: URL-backed attachment is available at https://sp/deck.pptx"},
		},
		{
			name: "single-char content hash is skipped",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return []query.AttachmentInfo{{Filename: "file2.txt", ContentHash: "a"}}
			},
			wantSubstrings: []string{"file2.txt: invalid content hash"},
		},
		{
			name: "mixed short hashes all reported",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return []query.AttachmentInfo{
					{Filename: "file.txt", ContentHash: ""},
					{Filename: "file2.txt", ContentHash: "a"},
				}
			},
			wantSubstrings: []string{
				"No attachments exported",
				"file.txt: invalid content hash",
				"file2.txt: invalid content hash",
			},
		},
		{
			name: "nil inputs produces no panic",
			setup: func(_ *testing.T, _ string) []query.AttachmentInfo {
				return nil
			},
			wantSubstrings: []string{"No attachments exported"},
		},
		{
			name: "mix of valid and invalid attachments",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
				t.Helper()
				hash := createAttachmentFile(t, attachDir, []byte("good content"))
				return []query.AttachmentInfo{
					{Filename: "bad.txt", ContentHash: ""},
					{Filename: "good.txt", ContentHash: hash},
				}
			},
			wantCount:      1,
			wantSubstrings: []string{"Exported 1 attachment(s)", "bad.txt: invalid content hash"},
			wantFiles:      []string{"good.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			attachDir := t.TempDir()
			zipPath := filepath.Join(t.TempDir(), "test.zip")

			inputs := tt.setup(t, attachDir)
			stats := Attachments(zipPath, attachDir, inputs)

			require.Equal(tt.wantCount, stats.Count, "Attachments() count")

			formatted := FormatExportResult(stats)
			for _, sub := range tt.wantSubstrings {
				assert.Contains(formatted, sub)
			}

			// Verify exported files exist in the zip
			if len(tt.wantFiles) > 0 {
				zr, err := zip.OpenReader(zipPath)
				require.NoError(err, "failed to open zip")
				defer func() { _ = zr.Close() }()

				zipEntries := make(map[string]bool)
				for _, f := range zr.File {
					zipEntries[f.Name] = true
				}
				for _, want := range tt.wantFiles {
					assert.True(zipEntries[want],
						"expected file %q in zip, got entries: %v", want, zipEntries)
				}
			}
		})
	}
}
