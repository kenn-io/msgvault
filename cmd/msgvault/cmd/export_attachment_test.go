package cmd

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// setupTestAttachment creates a temp dir with an attachment file stored using
// the content-addressed layout (hash[:2]/hash). Returns the attachments dir,
// the content hash, the file data, and a cleanup function.
func setupTestAttachment(t *testing.T) (string, string, []byte, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "msgvault-export-att-*")
	requirepkg.NoError(t, err, "create temp dir")

	contentHash := "61ccf192b5bd358738802dc2676d3ceab856f47d26dd29681ac3d335bfd5bbd0"
	data := []byte("test attachment content")

	subDir := filepath.Join(tmpDir, contentHash[:2])
	if err := os.MkdirAll(subDir, 0755); err != nil {
		_ = os.RemoveAll(tmpDir)
		requirepkg.NoError(t, err, "create subdir")
	}
	if err := os.WriteFile(filepath.Join(subDir, contentHash), data, 0600); err != nil {
		_ = os.RemoveAll(tmpDir)
		requirepkg.NoError(t, err, "write test file")
	}

	return tmpDir, contentHash, data, func() { _ = os.RemoveAll(tmpDir) }
}

func TestExportAttachment_BinaryToFile(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	attDir, contentHash, wantData, cleanup := setupTestAttachment(t)
	defer cleanup()

	outFile := filepath.Join(attDir, "output.bin")
	storagePath := filepath.Join(attDir, contentHash[:2], contentHash)

	// Reset global flag state
	exportAttachmentOutput = outFile
	exportAttachmentJSON = false
	exportAttachmentBase64 = false
	defer func() { exportAttachmentOutput = "" }()

	require.NoError(exportAttachmentBinary(storagePath, contentHash), "exportAttachmentBinary")

	got, err := os.ReadFile(outFile)
	require.NoError(err, "read output")
	assert.Equal(string(wantData), string(got), "output")

	// Verify file permissions (Windows does not support Unix permissions)
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(outFile)
		assert.Equal(os.FileMode(0600), info.Mode().Perm(), "file permissions")
	}
}

func TestExportAttachment_JSONOutput(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	attDir, contentHash, wantData, cleanup := setupTestAttachment(t)
	defer cleanup()

	storagePath := filepath.Join(attDir, contentHash[:2], contentHash)

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := exportAttachmentAsJSON(storagePath, contentHash)
	_ = w.Close()
	os.Stdout = oldStdout

	require.NoError(err, "exportAttachmentAsJSON")

	var result map[string]any
	require.NoError(json.NewDecoder(r).Decode(&result), "decode JSON")

	assert.Equal(contentHash, result["content_hash"], "content_hash")
	assert.Equal(len(wantData), int(result["size"].(float64)), "size")

	decoded, err := base64.StdEncoding.DecodeString(result["data_base64"].(string))
	require.NoError(err, "decode base64")
	assert.Equal(string(wantData), string(decoded), "decoded data")
}

func TestExportAttachment_Base64Output(t *testing.T) {
	attDir, contentHash, wantData, cleanup := setupTestAttachment(t)
	defer cleanup()

	storagePath := filepath.Join(attDir, contentHash[:2], contentHash)

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := exportAttachmentAsBase64(storagePath)
	_ = w.Close()
	os.Stdout = oldStdout

	requirepkg.NoError(t, err, "exportAttachmentAsBase64")

	outputBytes, _ := io.ReadAll(r)
	output := string(outputBytes)

	// Strip trailing newline
	expected := base64.StdEncoding.EncodeToString(wantData) + "\n"
	assertpkg.Equal(t, expected, output, "base64 output")
}

func TestExportAttachment_MissingFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "msgvault-export-att-missing-*")
	requirepkg.NoError(t, err, "create temp dir")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	storagePath := filepath.Join(tmpDir, hash[:2], hash)

	_, err = openAttachmentFile(storagePath)
	requirepkg.Error(t, err, "expected error for missing file")
	assertpkg.ErrorContains(t, err, "attachment not found")
}

func TestExportAttachment_FlagMutualExclusivity(t *testing.T) {
	tests := []struct {
		name   string
		output string
		json   bool
		base64 bool
		errMsg string
	}{
		{"json+base64", "", true, true, "--json and --base64 are mutually exclusive"},
		{"json+output", "file.bin", true, false, "--json and --output are mutually exclusive"},
		{"base64+output", "file.bin", false, true, "--base64 and --output are mutually exclusive"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exportAttachmentOutput = tc.output
			exportAttachmentJSON = tc.json
			exportAttachmentBase64 = tc.base64
			defer func() {
				exportAttachmentOutput = ""
				exportAttachmentJSON = false
				exportAttachmentBase64 = false
			}()

			// Use a valid hash — flag validation happens before file access
			err := runExportAttachment(nil, []string{
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			})

			requirepkg.Error(t, err, "expected error containing %q", tc.errMsg)
			assertpkg.ErrorContains(t, err, tc.errMsg)
		})
	}
}

func TestExportAttachment_HashValidation(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{"too short", "61ccf192"},
		{"too long", "61ccf192b5bd358738802dc2676d3ceab856f47d26dd29681ac3d335bfd5bbd0aa"},
		{"invalid hex", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"empty", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exportAttachmentOutput = ""
			exportAttachmentJSON = false
			exportAttachmentBase64 = false

			err := runExportAttachment(nil, []string{tc.hash})
			requirepkg.Error(t, err, "expected error for invalid hash")
			assertpkg.ErrorContains(t, err, "invalid content hash")
		})
	}
}
