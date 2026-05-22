package export

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

// createAttachmentFile creates a file in the content-addressed storage layout
// (root/<hash[:2]>/<hash>) and returns the SHA-256 hex hash of the content.
func createAttachmentFile(t *testing.T, root string, content []byte) string {
	t.Helper()
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	dir := filepath.Join(root, hash[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hash), content, 0o644); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestFormatExportResult_WriteErrorWithCount(t *testing.T) {
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
	if !strings.Contains(result, "Export failed due to write errors") {
		t.Errorf("expected failure message, got: %s", result)
	}
	if !strings.Contains(result, "Zip file removed") {
		t.Errorf("expected 'Zip file removed', got: %s", result)
	}
	if strings.Contains(result, "Exported 5 attachment") {
		t.Errorf("should not report success count when WriteError is true, got: %s", result)
	}
	if strings.Contains(result, "Saved to:") {
		t.Errorf("should not show 'Saved to:' when WriteError is true, got: %s", result)
	}
}

func TestPathTraversalInContentHash(t *testing.T) {
	// This test verifies that malicious content hashes with path traversal
	// sequences are rejected and cannot be used to read arbitrary files.

	// Create a "secret" file outside the attachments directory
	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "secret.txt")
	secretContent := []byte("TOP SECRET DATA")
	if err := os.WriteFile(secretFile, secretContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Create attachments directory (subdirectory to enable traversal)
	attachDir := filepath.Join(t.TempDir(), "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		t.Fatal(err)
	}

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
	if stats.Count > 0 {
		t.Errorf("path traversal attack succeeded: exported %d files", stats.Count)
	}

	// Should report an error about the invalid content hash
	found := false
	for _, errMsg := range stats.Errors {
		if strings.Contains(errMsg, "invalid content hash") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'invalid content hash' error, got errors: %v", stats.Errors)
	}
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
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateContentHash(%q) error = %v, wantErr %v", tt.hash, err, tt.wantErr)
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
				hash := createAttachmentFile(t, attachDir, []byte("hello world"))
				return []query.AttachmentInfo{{Filename: "greeting.txt", ContentHash: hash}}
			},
			wantFiles: 1,
			wantNames: []string{"greeting.txt"},
		},
		{
			name: "multiple files exported",
			setup: func(t *testing.T, attachDir string) []query.AttachmentInfo {
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
			attachDir := t.TempDir()
			outputDir := t.TempDir()

			inputs := tt.setup(t, attachDir)
			result := AttachmentsToDir(outputDir, attachDir, inputs)

			if got := len(result.Files); got != tt.wantFiles {
				t.Fatalf("got %d files, want %d; errors: %v", got, tt.wantFiles, result.Errors)
			}
			if got := len(result.Errors); got != tt.wantErrors {
				t.Fatalf("got %d errors, want %d; errors: %v", got, tt.wantErrors, result.Errors)
			}

			// Verify expected filenames
			for i, wantName := range tt.wantNames {
				if i >= len(result.Files) {
					break
				}
				gotName := filepath.Base(result.Files[i].Path)
				if gotName != wantName {
					t.Errorf("file[%d] name = %q, want %q", i, gotName, wantName)
				}
			}

			// Verify all exported files exist on disk and have correct content size
			for _, f := range result.Files {
				info, err := os.Stat(f.Path)
				if err != nil {
					t.Errorf("exported file %s does not exist: %v", f.Path, err)
					continue
				}
				if info.Size() != f.Size {
					t.Errorf("file %s size = %d, want %d", f.Path, info.Size(), f.Size)
				}
			}
		})
	}
}

func TestAttachmentsToDir_FilePermissions(t *testing.T) {
	attachDir := t.TempDir()
	outputDir := t.TempDir()

	hash := createAttachmentFile(t, attachDir, []byte("secret content"))
	inputs := []query.AttachmentInfo{{Filename: "doc.pdf", ContentHash: hash}}

	result := AttachmentsToDir(outputDir, attachDir, inputs)
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(result.Files))
	}

	// Windows does not support Unix permissions.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(result.Files[0].Path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("file permissions = %o, want 0600", perm)
		}
	}
}

func TestAttachmentsToDir_DiskConflict(t *testing.T) {
	// Pre-existing file on disk should trigger _1 suffix
	attachDir := t.TempDir()
	outputDir := t.TempDir()

	// Create a pre-existing file
	if err := os.WriteFile(filepath.Join(outputDir, "report.pdf"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	hash := createAttachmentFile(t, attachDir, []byte("new content"))
	inputs := []query.AttachmentInfo{{Filename: "report.pdf", ContentHash: hash}}

	result := AttachmentsToDir(outputDir, attachDir, inputs)
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d; errors: %v", len(result.Files), result.Errors)
	}

	gotName := filepath.Base(result.Files[0].Path)
	if gotName != "report_1.pdf" {
		t.Errorf("expected report_1.pdf, got %s", gotName)
	}

	// Verify original file is untouched
	orig, _ := os.ReadFile(filepath.Join(outputDir, "report.pdf"))
	if string(orig) != "old" {
		t.Error("original file was overwritten")
	}
}

func TestCreateExclusiveFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("new file", func(t *testing.T) {
		p := filepath.Join(dir, "new.txt")
		f, path, err := CreateExclusiveFile(p, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if path != p {
			t.Errorf("path = %q, want %q", path, p)
		}
		// Windows does not support Unix permissions.
		if runtime.GOOS != "windows" {
			info, _ := os.Stat(path)
			if perm := info.Mode().Perm(); perm != 0600 {
				t.Errorf("permissions = %o, want 0600", perm)
			}
		}
	})

	t.Run("conflict appends suffix", func(t *testing.T) {
		p := filepath.Join(dir, "existing.txt")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}

		f, path, err := CreateExclusiveFile(p, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if filepath.Base(path) != "existing_1.txt" {
			t.Errorf("path = %q, want existing_1.txt", filepath.Base(path))
		}
	})

	t.Run("multiple conflicts", func(t *testing.T) {
		p := filepath.Join(dir, "multi.txt")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "multi_1.txt"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}

		f, path, err := CreateExclusiveFile(p, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if filepath.Base(path) != "multi_2.txt" {
			t.Errorf("path = %q, want multi_2.txt", filepath.Base(path))
		}
	})

	t.Run("no extension", func(t *testing.T) {
		p := filepath.Join(dir, "noext")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}

		f, path, err := CreateExclusiveFile(p, 0644)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if filepath.Base(path) != "noext_1" {
			t.Errorf("path = %q, want noext_1", filepath.Base(path))
		}
	})
}

func TestValidateOutputPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"simple filename", "invoice.pdf", false},
		{"filename with dot prefix", "./invoice.pdf", false},
		{"subdirectory", "subdir/file.pdf", false},
		{"stdout dash", "-", false},
		{"dot-dot prefix filename", "..backup", false},
		{"dot-dot middle filename", "foo..bar.txt", false},

		// Rooted/absolute paths (could be malicious attachment names)
		{"absolute unix path", "/tmp/file.pdf", true},
		{"absolute path with traversal", "/etc/cron.d/evil", true},
		{"backslash rooted path", `\tmp\file.pdf`, true},

		// Path traversal attacks (e.g., from email-supplied filenames)
		{"parent traversal", "../evil.txt", true},
		{"deep traversal", "../../.ssh/authorized_keys", true},
		{"traversal via subdir", "foo/../../evil.txt", true},
	}

	// Windows drive and UNC paths — only meaningful on Windows where
	// filepath.VolumeName returns non-empty for these forms.
	if runtime.GOOS == "windows" {
		tests = append(tests,
			struct {
				name    string
				path    string
				wantErr bool
			}{"windows absolute", `C:\tmp\file.pdf`, true},
			struct {
				name    string
				path    string
				wantErr bool
			}{"windows drive-relative", `C:tmp\file.pdf`, true},
			struct {
				name    string
				path    string
				wantErr bool
			}{"windows drive-relative traversal", `C:..\evil`, true},
			struct {
				name    string
				path    string
				wantErr bool
			}{"windows UNC path", `\\server\share\file.pdf`, true},
		)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOutputPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateOutputPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
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
			attachDir := t.TempDir()
			zipPath := filepath.Join(t.TempDir(), "test.zip")

			inputs := tt.setup(t, attachDir)
			stats := Attachments(zipPath, attachDir, inputs)

			if stats.Count != tt.wantCount {
				t.Fatalf("Attachments() count = %d, want %d", stats.Count, tt.wantCount)
			}

			formatted := FormatExportResult(stats)
			testutil.AssertContainsAll(t, formatted, tt.wantSubstrings)

			// Verify exported files exist in the zip
			if len(tt.wantFiles) > 0 {
				zr, err := zip.OpenReader(zipPath)
				if err != nil {
					t.Fatalf("failed to open zip: %v", err)
				}
				defer func() { _ = zr.Close() }()

				zipEntries := make(map[string]bool)
				for _, f := range zr.File {
					zipEntries[f.Name] = true
				}
				for _, want := range tt.wantFiles {
					if !zipEntries[want] {
						t.Errorf("expected file %q in zip, got entries: %v", want, zipEntries)
					}
				}
			}
		})
	}
}
