package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/store"
)

const hexRecordingEndpointPath = "/api/v1/import/hex/recording"

type HexRecordingImporter interface {
	ImportHexRecording(ctx context.Context, upload hex.RecordingUpload, input io.Reader, maxBytes int64) (bool, error)
}

func (s *Server) registerHexRecordingRoute(api huma.API) {
	op := rawAPIV1Operation("importHexRecording", http.MethodPost, "/import/hex/recording", "Stream retained audio for an archived Hex meeting")
	for _, name := range []string{"track", "identifier", "account_email", "message_id", "sha256", "size", "no_build_cache"} {
		typ := huma.TypeString
		if name == "message_id" || name == "size" {
			typ = huma.TypeInteger
		}
		if name == "no_build_cache" {
			typ = huma.TypeBoolean
		}
		schema := &huma.Schema{Type: typ}
		if name == "track" {
			schema.Enum = []any{"system", "microphone"}
		}
		op.Parameters = append(op.Parameters, &huma.Param{Name: name, In: "query", Required: name != "no_build_cache", Schema: schema})
	}
	op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
		"audio/wav": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
	}}
	op.Responses = jsonResponsesFor[hex.RemoteResult](api, http.StatusOK)
	op.Errors = []int{400, 401, 403, 404, 413, 415, 422, 500, 503}
	registerRawHumaRoute(api, op, s.handleHexRecording)
}

func (s *Server) handleHexRecording(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "audio/wav" {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be audio/wav")
		return
	}
	query := r.URL.Query()
	messageID, idErr := strconv.ParseInt(query.Get("message_id"), 10, 64)
	size, sizeErr := strconv.ParseInt(query.Get("size"), 10, 64)
	skip := false
	if value := query.Get("no_build_cache"); value != "" {
		skip, err = strconv.ParseBool(value)
	}
	upload := hex.RecordingUpload{Source: meetingimport.Source{Identifier: query.Get("identifier"), AccountEmail: query.Get("account_email")},
		MessageID: messageID, Size: size, Hash: query.Get("sha256"), Track: query.Get("track"), MIMEType: mediaType, NoBuildCache: skip}
	var normalizeErr error
	upload, normalizeErr = upload.Normalize()
	if idErr != nil || sizeErr != nil || err != nil || normalizeErr != nil {
		writeError(w, 422, "record_validation_failed", "Recording metadata failed validation")
		return
	}
	capBytes := s.cfg.Server.HexRecordingMaxBytes
	if capBytes <= 0 {
		capBytes = meetingarchive.DefaultRecordingMaxBytes
	}
	if size > capBytes || r.ContentLength > capBytes {
		writeError(w, 413, "recording_too_large", meetingarchive.ErrRecordingTooLarge.Error())
		return
	}
	importer, ok := s.store.(HexRecordingImporter)
	if !ok || importer == nil {
		writeError(w, 503, "service_unavailable", "Hex recording import is unavailable")
		return
	}
	staged, err := hex.StageRecordingUpload(r.Context(), upload, r.Body, capBytes)
	if err != nil {
		s.writeHexRecordingImportError(w, err)
		return
	}
	defer func() { _ = staged.Close() }()

	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, "Hex recording import")
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	changed, err := importer.ImportHexRecording(r.Context(), upload, io.NewSectionReader(staged.File, 0, staged.Size), capBytes)
	if err != nil {
		s.writeHexRecordingImportError(w, err)
		return
	}
	status := "unchanged"
	if changed {
		status = "updated"
	}
	writeJSON(w, http.StatusOK, hex.RemoteResult{Status: status, MessageID: messageID, Changed: changed})
}

func (s *Server) writeHexRecordingImportError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, meetingarchive.ErrRecordingTooLarge):
		writeError(w, 413, "recording_too_large", meetingarchive.ErrRecordingTooLarge.Error())
	case errors.Is(err, hex.ErrRemoteValidation), errors.Is(err, meetingarchive.ErrRecordingChanged):
		writeError(w, 422, "record_validation_failed", "Recording failed validation; sync again")
	case errors.Is(err, store.ErrSourceNotFound):
		writeError(w, 404, "source_not_found", "Hex source is not registered")
	default:
		writeError(w, 500, "internal_error", "Hex recording import failed")
	}
}
