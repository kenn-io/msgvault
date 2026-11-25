package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

type recorderImportErrors struct {
	name                                 string
	malformed, large, validation, record error
}

func serveRecorderImport[Q, R any](s *Server, w http.ResponseWriter, r *http.Request, path string, e recorderImportErrors, decode func(io.Reader, int64) (Q, error), importer func(context.Context, Q) (R, error), created func(R) bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != applicationJSONMediaType {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	request, err := decode(r.Body, meetingimport.MaxRequestBytes)
	switch {
	case errors.Is(err, e.large):
		writeError(w, 413, "request_too_large", e.name+" transfer exceeds 16 MiB")
		return
	case errors.Is(err, e.malformed):
		writeError(w, 400, "bad_request", "Invalid "+e.name+" transfer JSON")
		return
	case err != nil:
		writeError(w, 422, "validation_failed", e.name+" transfer failed validation")
		return
	}
	if importer == nil {
		writeError(w, 503, "service_unavailable", e.name+" import is unavailable")
		return
	}
	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, operationGateLabelFromPath(path))
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	result, err := importer(r.Context(), request)
	if err != nil {
		s.writeRecorderImportError(w, err, e, false)
		return
	}
	status := http.StatusOK
	if created(result) {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
func (s *Server) writeRecorderImportError(w http.ResponseWriter, err error, e recorderImportErrors, recording bool) {
	if s.writeIfContextError(w, err) {
		return
	}
	if recording {
		switch {
		case errors.Is(err, meetingarchive.ErrRecordingTooLarge):
			writeError(w, 413, "recording_too_large", meetingarchive.ErrRecordingTooLarge.Error())
		case errors.Is(err, e.validation), errors.Is(err, meetingarchive.ErrRecordingChanged):
			writeError(w, 422, "record_validation_failed", "Recording failed validation; sync again")
		case errors.Is(err, store.ErrSourceNotFound):
			writeError(w, 404, "source_not_found", e.name+" source is not registered")
		default:
			writeError(w, 500, "internal_error", e.name+" recording import failed")
		}
		return
	}
	switch {
	case errors.Is(err, e.record):
		writeError(w, 422, "record_validation_failed", e.name+" meeting failed validation")
	case errors.Is(err, e.validation):
		writeError(w, 422, "validation_failed", e.name+" transfer failed validation")
	case errors.Is(err, store.ErrSourceNotFound):
		writeError(w, 404, "source_not_found", e.name+" source is not registered; run msgvault add-"+strings.ToLower(e.name)+" first")
	default:
		if s.logger != nil {
			s.logger.Error(e.name+" import failed", "error_class", "internal")
		}
		writeError(w, 500, "internal_error", e.name+" import failed")
	}
}

type recorderRecordingClaims struct {
	Source                meetingimport.Source
	MessageID, Size       int64
	Hash, MIMEType, Track string
	NoBuildCache          bool
}

// serveRecorderRecording stages bounded audio before acquiring the archive gate.
func serveRecorderRecording[U any](s *Server, w http.ResponseWriter, r *http.Request, e recorderImportErrors, capBytes int64, mediaTypes []string, normalize func(recorderRecordingClaims) (U, error), stage func(context.Context, U, io.Reader, int64) (*meetingarchive.Recording, error), importer func(context.Context, U, io.Reader, int64) (bool, error)) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	allowed := false
	for _, kind := range mediaTypes {
		allowed = allowed || kind == media
	}
	if err != nil || !allowed {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be "+strings.Join(mediaTypes, " or "))
		return
	}
	query := r.URL.Query()
	id, idErr := strconv.ParseInt(query.Get("message_id"), 10, 64)
	size, sizeErr := strconv.ParseInt(query.Get("size"), 10, 64)
	skip := false
	if value := query.Get("no_build_cache"); value != "" {
		skip, err = strconv.ParseBool(value)
	}
	upload, normalizeErr := normalize(recorderRecordingClaims{Source: meetingimport.Source{Identifier: query.Get("identifier"), AccountEmail: query.Get("account_email")}, MessageID: id, Size: size, Hash: query.Get("sha256"), MIMEType: media, Track: query.Get("track"), NoBuildCache: skip})
	if idErr != nil || sizeErr != nil || err != nil || normalizeErr != nil {
		writeError(w, 422, "record_validation_failed", "Recording metadata failed validation")
		return
	}
	if capBytes <= 0 {
		capBytes = meetingarchive.DefaultRecordingMaxBytes
	}
	if size > capBytes || r.ContentLength > capBytes {
		s.writeRecorderImportError(w, meetingarchive.ErrRecordingTooLarge, e, true)
		return
	}
	if importer == nil {
		writeError(w, 503, "service_unavailable", e.name+" recording import is unavailable")
		return
	}
	staged, err := stage(r.Context(), upload, r.Body, capBytes)
	if err != nil {
		s.writeRecorderImportError(w, err, e, true)
		return
	}
	defer func() { _ = staged.Close() }()
	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, e.name+" recording import")
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	changed, err := importer(r.Context(), upload, io.NewSectionReader(staged.File, 0, staged.Size), capBytes)
	if err != nil {
		s.writeRecorderImportError(w, err, e, true)
		return
	}
	status := "unchanged"
	if changed {
		status = "updated"
	}
	writeJSON(w, http.StatusOK, struct {
		Status    string `json:"status"`
		SourceID  int64  `json:"source_id"`
		MessageID int64  `json:"message_id"`
		Changed   bool   `json:"changed"`
	}{status, 0, id, changed})
}
