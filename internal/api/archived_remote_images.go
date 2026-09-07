package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"

	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
)

type archivedRemoteImageReader interface {
	MessageRemoteImages(messageID int64) (map[string]store.AttachmentRef, error)
}

func (s *Server) archivedRemoteImageHTML(id int64, body string) string {
	reader, ok := s.store.(archivedRemoteImageReader)
	if !ok || body == "" {
		return body
	}
	refs, err := reader.MessageRemoteImages(id)
	if err != nil {
		s.logger.Warn("cannot read archived remote images", "message", id, "error", err)
		return body
	}
	return remoteimage.RewriteHTML(body, refs)
}

// serveArchivedRemoteImage resolves a CID through the owning message before
// accessing CAS. Possession of a digest is never enough to select another
// message's image, and no missing image triggers an outbound request.
func (s *Server) serveArchivedRemoteImage(w http.ResponseWriter, r *http.Request, id int64, cid string) {
	reader, ok := s.store.(archivedRemoteImageReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Archived images unavailable")
		return
	}
	refs, err := reader.MessageRemoteImages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot read archived images")
		return
	}
	ref, ok := refs[cid]
	if !ok || ref.ContentID != cid || ref.StoragePath == "" || msgexport.ValidateContentHash(ref.ContentHash) != nil {
		writeError(w, http.StatusNotFound, "not_found", "Archived image not found")
		return
	}
	var stream io.ReadCloser
	if s.blobStore != nil {
		stream, _, err = s.blobStore.OpenStream(r.Context(), ref.ContentHash)
	} else if s.cfg != nil {
		var path string
		path, err = msgexport.StoragePath(s.cfg.AttachmentsDir(), ref.ContentHash)
		if err == nil {
			// The hash was validated and StoragePath anchors it in configured CAS.
			stream, err = os.Open(path)
		}
	} else {
		err = errors.New("attachment store unavailable")
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Archived image bytes unavailable")
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(stream, remoteimage.MaxImageBytes+1))
	if err := errors.Join(readErr, stream.Close()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot read archived image")
		return
	}
	if len(body) > remoteimage.MaxImageBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Archived image exceeds size cap")
		return
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != ref.ContentHash {
		writeError(w, http.StatusInternalServerError, "invalid_content", "Archived image bytes failed verification")
		return
	}
	switch ref.MimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "Archived image type not permitted")
		return
	}
	if http.DetectContentType(body) != ref.MimeType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "Archived image type does not match its bytes")
		return
	}
	w.Header().Set("Content-Type", ref.MimeType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}
