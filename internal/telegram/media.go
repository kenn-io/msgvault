package telegram

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/export"
)

// telegramAttachmentKeyPrefix namespaces synthetic attachment hashes so a
// missing-media placeholder can never be mistaken for a SHA-256 of stored
// bytes.
const telegramAttachmentKeyPrefix = "telegram:attachment:"

// attachmentCandidate is one media path a message carries.
type attachmentCandidate struct {
	// RelPath is the export-relative path from the JSON.
	RelPath string
	// Filename is the display filename for the row.
	Filename string
	// MIMEType is the export's mime_type when it carries one; empty
	// otherwise, and the caller falls back to the filename's extension.
	MIMEType string
	// MediaType labels the row (photo, video_file, sticker…).
	MediaType string
}

// attachmentCandidates lists a message's media in export order: photo,
// file, then thumbnail.
func attachmentCandidates(m *Message) []attachmentCandidate {
	var out []attachmentCandidate
	if m.Photo != "" {
		out = append(out, attachmentCandidate{
			RelPath: m.Photo, Filename: filepath.Base(m.Photo), MediaType: mediaTypeOr(m, "photo"),
		})
	}
	if m.File != "" {
		// The export's file_name and mime_type are authoritative document
		// metadata; the storage basename and title are fallbacks.
		filename := m.FileName
		if filename == "" {
			filename = m.Title
		}
		if filename == "" {
			filename = filepath.Base(m.File)
		}
		out = append(out, attachmentCandidate{
			RelPath: m.File, Filename: filename, MIMEType: m.MimeType, MediaType: mediaTypeOr(m, ""),
		})
	}
	if m.Thumbnail != "" {
		out = append(out, attachmentCandidate{
			RelPath: m.Thumbnail, Filename: filepath.Base(m.Thumbnail), MediaType: "thumbnail",
		})
	}
	return out
}

func mediaTypeOr(m *Message, fallback string) string {
	if m.MediaType != "" {
		return m.MediaType
	}
	return fallback
}

// resolveMediaPath joins an export-relative media path onto the export root
// and resolves symlinks on both sides before containment: the joined path
// and the root are evaluated, and a resolved path outside the resolved root
// is rejected. Telegram writes forward-separated relative paths; a hostile
// export could otherwise point a symlink at an arbitrary local file.
func resolveMediaPath(root, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("telegram: empty media path")
	}
	if strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("telegram: media path %q contains NUL", rel)
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("telegram: media path %q is absolute", rel)
	}
	joined := filepath.Join(root, filepath.FromSlash(clean))
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("telegram: resolve export root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("telegram: resolve media path %q: %w", rel, err)
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("telegram: media path %q escapes the export directory", rel)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("telegram: stat media path %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("telegram: media path %q is not a regular file", rel)
	}
	return resolved, nil
}

// storeAttachment streams one media file into content-addressed storage. It
// returns the storage path, content hash, and size on success; on any
// failure the storage path is empty and the error says why, so the caller
// can still record a metadata-only row (attachmentsDir "" disables storage
// the same way).
func storeAttachment(attachmentsDir, exportRoot, relPath string) (storagePath, contentHash string, size int64, err error) {
	if attachmentsDir == "" {
		return "", "", 0, nil
	}
	src, err := resolveMediaPath(exportRoot, relPath)
	if err != nil {
		return "", "", 0, err
	}
	return export.StoreAttachmentFromPath(attachmentsDir, src, 0)
}

// syntheticAttachmentHash derives a stable key for an attachment whose bytes
// are unavailable, keyed on the message-scoped media path so re-imports
// collapse to the same row.
func syntheticAttachmentHash(chatID, messageID int64, relPath string) string {
	key := fmt.Sprintf("%d:%d:%s", chatID, messageID, relPath)
	sum := sha256.Sum256([]byte(telegramAttachmentKeyPrefix + key))
	return telegramAttachmentKeyPrefix + hex.EncodeToString(sum[:])
}

// mimeTypeFor guesses a MIME type from a media path's extension.
func mimeTypeFor(path string) string {
	if ext := filepath.Ext(path); ext != "" {
		if mt := mime.TypeByExtension(strings.ToLower(ext)); mt != "" {
			return mt
		}
	}
	return "application/octet-stream"
}
