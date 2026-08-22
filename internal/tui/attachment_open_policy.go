package tui

import (
	"fmt"
	"mime"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gabriel-vasile/mimetype"
	"go.kenn.io/msgvault/internal/query"
)

type attachmentOpenType struct {
	declaredMIMEs []string
	detectedMIMEs []string
}

// safeAttachmentOpenTypes is deliberately an allowlist. Scriptable,
// executable, archive, installer, shortcut, macro-enabled Office, and unknown
// formats remain download-only. Each entry pins both the sender-declared MIME
// type and the MIME type detected from the downloaded bytes to the canonical
// filename extension.
var safeAttachmentOpenTypes = map[string]attachmentOpenType{
	".pdf":  {declaredMIMEs: []string{"application/pdf"}, detectedMIMEs: []string{"application/pdf"}},
	".txt":  {declaredMIMEs: []string{"text/plain"}, detectedMIMEs: []string{"text/plain"}},
	".md":   {declaredMIMEs: []string{"text/markdown", "text/plain"}, detectedMIMEs: []string{"text/plain"}},
	".png":  {declaredMIMEs: []string{"image/png"}, detectedMIMEs: []string{"image/png", "image/vnd.mozilla.apng"}},
	".jpg":  {declaredMIMEs: []string{"image/jpeg"}, detectedMIMEs: []string{"image/jpeg"}},
	".jpeg": {declaredMIMEs: []string{"image/jpeg"}, detectedMIMEs: []string{"image/jpeg"}},
	".gif":  {declaredMIMEs: []string{"image/gif"}, detectedMIMEs: []string{"image/gif"}},
	".webp": {declaredMIMEs: []string{"image/webp"}, detectedMIMEs: []string{"image/webp"}},
	".bmp":  {declaredMIMEs: []string{"image/bmp"}, detectedMIMEs: []string{"image/bmp"}},
	".tif":  {declaredMIMEs: []string{"image/tiff"}, detectedMIMEs: []string{"image/tiff"}},
	".tiff": {declaredMIMEs: []string{"image/tiff"}, detectedMIMEs: []string{"image/tiff"}},
	".mp3":  {declaredMIMEs: []string{"audio/mpeg"}, detectedMIMEs: []string{"audio/mpeg"}},
	".m4a":  {declaredMIMEs: []string{"audio/mp4", "audio/x-m4a"}, detectedMIMEs: []string{"audio/mp4", "audio/x-m4a"}},
	".wav":  {declaredMIMEs: []string{"audio/wav", "audio/x-wav"}, detectedMIMEs: []string{"audio/wav"}},
	".flac": {declaredMIMEs: []string{"audio/flac"}, detectedMIMEs: []string{"audio/flac"}},
	".ogg":  {declaredMIMEs: []string{"audio/ogg"}, detectedMIMEs: []string{"application/ogg", "audio/ogg"}},
	".opus": {declaredMIMEs: []string{"audio/opus", "audio/ogg"}, detectedMIMEs: []string{"application/ogg", "audio/ogg"}},
	".mp4":  {declaredMIMEs: []string{"video/mp4"}, detectedMIMEs: []string{"video/mp4"}},
	".mov":  {declaredMIMEs: []string{"video/quicktime"}, detectedMIMEs: []string{"video/quicktime"}},
	".webm": {declaredMIMEs: []string{"video/webm"}, detectedMIMEs: []string{"video/webm"}},
	".mkv":  {declaredMIMEs: []string{"video/x-matroska"}, detectedMIMEs: []string{"video/x-matroska"}},
}

func validateAttachmentOpen(att query.AttachmentInfo) error {
	extension := strings.ToLower(filepath.Ext(att.Filename))
	openType, ok := safeAttachmentOpenTypes[extension]
	if !ok {
		if extension == "" {
			extension = "without a canonical extension"
		}
		return fmt.Errorf("attachment %q is download-only (%s)", att.Filename, extension)
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(att.MimeType))
	if err != nil || !slices.Contains(openType.declaredMIMEs, strings.ToLower(mediaType)) {
		return fmt.Errorf(
			"attachment %q is download-only: MIME type %q does not match canonical extension %s",
			att.Filename, att.MimeType, extension,
		)
	}
	return nil
}

func validateDownloadedAttachmentOpen(att query.AttachmentInfo, path string) error {
	extension := strings.ToLower(filepath.Ext(att.Filename))
	openType, ok := safeAttachmentOpenTypes[extension]
	if !ok {
		return fmt.Errorf("attachment %q is download-only", att.Filename)
	}
	detected, err := mimetype.DetectFile(path)
	if err != nil {
		return fmt.Errorf("inspect downloaded attachment %q: %w", att.Filename, err)
	}
	if !slices.Contains(openType.detectedMIMEs, strings.ToLower(detected.String())) {
		return fmt.Errorf(
			"attachment %q is download-only: detected content type %q does not match canonical extension %s",
			att.Filename, detected.String(), extension,
		)
	}
	return nil
}
