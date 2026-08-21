package tui

import (
	"fmt"
	"mime"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/query"
)

// safeAttachmentOpenTypes is deliberately an allowlist. Scriptable,
// executable, archive, installer, shortcut, macro-enabled Office, and unknown
// formats remain download-only. Each entry also pins the canonical filename
// extension to the declared MIME type so an active payload cannot borrow a
// passive-looking name.
var safeAttachmentOpenTypes = map[string][]string{
	".pdf":  {"application/pdf"},
	".txt":  {"text/plain"},
	".md":   {"text/markdown", "text/plain"},
	".png":  {"image/png"},
	".jpg":  {"image/jpeg"},
	".jpeg": {"image/jpeg"},
	".gif":  {"image/gif"},
	".webp": {"image/webp"},
	".bmp":  {"image/bmp"},
	".tif":  {"image/tiff"},
	".tiff": {"image/tiff"},
	".mp3":  {"audio/mpeg"},
	".m4a":  {"audio/mp4", "audio/x-m4a"},
	".wav":  {"audio/wav", "audio/x-wav"},
	".flac": {"audio/flac"},
	".ogg":  {"audio/ogg"},
	".opus": {"audio/opus", "audio/ogg"},
	".mp4":  {"video/mp4"},
	".mov":  {"video/quicktime"},
	".webm": {"video/webm"},
	".mkv":  {"video/x-matroska"},
	".docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	".xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	".pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
}

func validateAttachmentOpen(att query.AttachmentInfo) error {
	extension := strings.ToLower(filepath.Ext(att.Filename))
	allowedMIMEs, ok := safeAttachmentOpenTypes[extension]
	if !ok {
		if extension == "" {
			extension = "without a canonical extension"
		}
		return fmt.Errorf("attachment %q is download-only (%s)", att.Filename, extension)
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(att.MimeType))
	if err != nil || !slices.Contains(allowedMIMEs, strings.ToLower(mediaType)) {
		return fmt.Errorf(
			"attachment %q is download-only: MIME type %q does not match canonical extension %s",
			att.Filename, att.MimeType, extension,
		)
	}
	return nil
}
