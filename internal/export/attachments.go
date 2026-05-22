// Package export handles file export operations such as creating zip archives
// of email attachments.
package export

import (
	"archive/zip"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go.kenn.io/msgvault/internal/fileutil"

	"go.kenn.io/msgvault/internal/query"
)

// ErrInvalidContentHash is returned when a content hash fails validation.
var ErrInvalidContentHash = errors.New("invalid content hash")

// ValidateContentHash validates that a content hash is a valid SHA-256 hex string.
// This prevents path traversal attacks by ensuring the hash contains only
// hexadecimal characters and is exactly 64 characters long.
func ValidateContentHash(hash string) error {
	// SHA-256 produces 32 bytes = 64 hex characters
	if len(hash) != 64 {
		return fmt.Errorf("%w: must be exactly 64 hex characters, got %d", ErrInvalidContentHash, len(hash))
	}

	// Verify all characters are valid hexadecimal
	_, err := hex.DecodeString(hash)
	if err != nil {
		return fmt.Errorf("%w: contains non-hexadecimal characters", ErrInvalidContentHash)
	}

	return nil
}

// ExportStats contains structured results of an attachment export operation.
type ExportStats struct {
	Count      int
	Size       int64
	Errors     []string
	ZipPath    string
	WriteError bool // true if a write error occurred and the zip was removed
}

// Attachments exports the given attachments into a zip file.
// It reads attachment content from attachmentsDir using content-hash based paths.
func Attachments(zipFilename, attachmentsDir string, attachments []query.AttachmentInfo) ExportStats {
	zipFile, err := os.Create(zipFilename)
	if err != nil {
		return ExportStats{Errors: []string{fmt.Sprintf("failed to create zip file: %v", err)}}
	}

	zipWriter := zip.NewWriter(zipFile)

	var stats ExportStats
	var writeError bool

	usedNames := make(map[string]int)
	for _, att := range attachments {
		if err := ValidateContentHash(att.ContentHash); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}

		n, err := addAttachmentToZip(zipWriter, attachmentsDir, att, usedNames)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			if isWriteError(err) {
				writeError = true
			}
			continue
		}

		stats.Count++
		stats.Size += n
	}

	if err := zipWriter.Close(); err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf("zip finalization error: %v", err))
		writeError = true
	}
	if err := zipFile.Close(); err != nil {
		stats.Errors = append(stats.Errors, fmt.Sprintf("file close error: %v", err))
		writeError = true
	}

	if stats.Count == 0 || writeError {
		_ = os.Remove(zipFilename)
		stats.WriteError = writeError
		return stats
	}

	cwd, _ := os.Getwd()
	stats.ZipPath = filepath.Join(cwd, zipFilename)
	return stats
}

// FormatExportResult formats ExportStats into a human-readable string for display.
func FormatExportResult(stats ExportStats) string {
	// Write error is fatal - zip was removed regardless of count
	if stats.WriteError {
		msg := "Export failed due to write errors. Zip file removed."
		if len(stats.Errors) > 0 {
			msg += "\n\nErrors:\n" + strings.Join(stats.Errors, "\n")
		}
		return msg
	}

	if stats.Count == 0 {
		msg := "No attachments exported."
		if len(stats.Errors) > 0 {
			msg += "\n\nErrors:\n" + strings.Join(stats.Errors, "\n")
		}
		return msg
	}

	result := fmt.Sprintf("Exported %d attachment(s) (%s)\n\nSaved to:\n%s",
		stats.Count, FormatBytesLong(stats.Size), stats.ZipPath)
	if len(stats.Errors) > 0 {
		result += "\n\nErrors:\n" + strings.Join(stats.Errors, "\n")
	}
	return result
}

type zipWriteError struct {
	err error
}

func (e *zipWriteError) Error() string { return e.err.Error() }
func (e *zipWriteError) Unwrap() error { return e.err }

func isWriteError(err error) bool {
	_, ok := err.(*zipWriteError)
	return ok
}

func addAttachmentToZip(zw *zip.Writer, root string, att query.AttachmentInfo, usedNames map[string]int) (int64, error) {
	storagePath, err := StoragePath(root, att.ContentHash)
	if err != nil {
		return 0, err
	}
	srcFile, err := os.Open(storagePath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = srcFile.Close() }()

	filename := resolveUniqueFilename(att.Filename, att.ContentHash, usedNames)

	w, err := zw.Create(filename)
	if err != nil {
		return 0, &zipWriteError{fmt.Errorf("zip write error: %w", err)}
	}

	n, err := io.Copy(w, srcFile)
	if err != nil {
		return 0, &zipWriteError{fmt.Errorf("zip write error: %w", err)}
	}

	return n, nil
}

func resolveUniqueFilename(original, contentHash string, usedNames map[string]int) string {
	filename := SanitizeFilename(filepath.Base(original))
	if filename == "" || filename == "." {
		filename = contentHash
	}

	baseKey := filename
	if count, exists := usedNames[baseKey]; exists {
		ext := filepath.Ext(filename)
		base := filename[:len(filename)-len(ext)]
		filename = fmt.Sprintf("%s_%d%s", base, count+1, ext)
		usedNames[baseKey] = count + 1
	} else {
		usedNames[baseKey] = 1
	}

	return filename
}

// SanitizeFilename removes or replaces characters that are invalid in filenames.
func SanitizeFilename(s string) string {
	var result []rune
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			result = append(result, '_')
		default:
			result = append(result, r)
		}
	}
	return string(result)
}

// ValidateOutputPath checks that an output file path does not escape the
// current working directory. This guards against email-supplied filenames
// being passed to --output (e.g., an attachment named
// "../../.ssh/authorized_keys" or "/etc/cron.d/evil").
// Both absolute paths and ".." traversal are rejected.
func ValidateOutputPath(outputPath string) error {
	cleaned := filepath.Clean(outputPath)
	if filepath.IsAbs(cleaned) {
		return fmt.Errorf("output path %q is absolute; use a relative path", outputPath)
	}
	// Reject Windows drive-relative (C:foo) and UNC (\\server\share) paths,
	// which filepath.IsAbs does not catch.
	if filepath.VolumeName(cleaned) != "" {
		return fmt.Errorf("output path %q contains a drive or UNC prefix; use a relative path", outputPath)
	}
	// Reject rooted paths (leading / or \) which are drive-relative on Windows
	// and absolute on Unix. filepath.IsAbs misses these on Windows.
	if len(cleaned) > 0 && (cleaned[0] == '/' || cleaned[0] == '\\') {
		return fmt.Errorf("output path %q is rooted; use a relative path", outputPath)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("output path %q escapes the working directory", outputPath)
	}
	return nil
}

// StoragePath returns the content-addressed file path for an attachment:
// attachmentsDir/<hash[:2]>/<hash>. Returns an error if the content hash
// is invalid (prevents panics from short/empty strings).
func StoragePath(attachmentsDir, contentHash string) (string, error) {
	if err := ValidateContentHash(contentHash); err != nil {
		return "", err
	}
	return filepath.Join(attachmentsDir, contentHash[:2], contentHash), nil
}

// ExportedFile represents a single file written to a directory.
type ExportedFile struct {
	Path string // absolute path of the written file
	Size int64
}

// DirExportResult contains the results of exporting attachments to a directory.
type DirExportResult struct {
	Files  []ExportedFile
	Errors []string
}

// TotalSize returns the sum of all exported file sizes.
func (r DirExportResult) TotalSize() int64 {
	var total int64
	for _, f := range r.Files {
		total += f.Size
	}
	return total
}

// AttachmentsToDir exports attachments as individual files into outputDir.
// It reads attachment content from attachmentsDir using content-hash based paths
// and writes each file with its original filename (sanitized, deduplicated).
// Files are created with O_EXCL to avoid overwriting existing files.
func AttachmentsToDir(outputDir, attachmentsDir string, attachments []query.AttachmentInfo) DirExportResult {
	var result DirExportResult
	usedNames := make(map[string]int)

	for _, att := range attachments {
		if err := ValidateContentHash(att.ContentHash); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}

		filename := resolveUniqueFilename(att.Filename, att.ContentHash, usedNames)
		exported, err := exportAttachmentToFile(outputDir, attachmentsDir, att.ContentHash, filename)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}

		result.Files = append(result.Files, exported)
	}

	return result
}

// exportAttachmentToFile streams a single attachment from content-addressed
// storage to outputDir/filename. Uses O_EXCL to avoid overwriting; appends
// _1, _2, etc. on conflict.
func exportAttachmentToFile(outputDir, attachmentsDir, contentHash, filename string) (ExportedFile, error) {
	srcPath, err := StoragePath(attachmentsDir, contentHash)
	if err != nil {
		return ExportedFile{}, err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ExportedFile{}, fmt.Errorf("attachment file not found for hash %s", contentHash)
		}
		return ExportedFile{}, fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = src.Close() }()

	destPath := filepath.Join(outputDir, filename)
	dst, finalPath, err := CreateExclusiveFile(destPath, 0600)
	if err != nil {
		return ExportedFile{}, fmt.Errorf("create output file: %w", err)
	}

	n, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(finalPath)
		return ExportedFile{}, fmt.Errorf("write: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(finalPath)
		return ExportedFile{}, fmt.Errorf("close: %w", closeErr)
	}

	return ExportedFile{Path: finalPath, Size: n}, nil
}

// CreateExclusiveFile atomically creates a file that doesn't already exist,
// trying path, path_1, path_2, etc. on conflict. Returns the open file and
// the path that was actually used. Uses O_CREATE|O_EXCL to avoid TOCTOU races.
func CreateExclusiveFile(p string, perm os.FileMode) (*os.File, string, error) {
	f, err := fileutil.SecureOpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err == nil {
		return f, p, nil
	}
	if !pathConflict(err) {
		return nil, "", err
	}
	ext := filepath.Ext(p)
	base := p[:len(p)-len(ext)]
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		f, err = fileutil.SecureOpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, candidate, nil
		}
		if !pathConflict(err) {
			return nil, "", err
		}
	}
}

// pathConflict reports whether err indicates the path already exists as a
// file or directory.
func pathConflict(err error) bool {
	return os.IsExist(err) || errors.Is(err, syscall.EISDIR)
}

// FormatBytesLong formats bytes with full precision for export results.
func FormatBytesLong(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
