package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
)

const muesliRecordingEndpointPath = "/api/v1/import/muesli/recording"

type MuesliRecordingImporter interface {
	ImportMuesliRecording(ctx context.Context, upload muesli.RecordingUpload, input io.Reader, maxBytes int64) (bool, error)
}

func (s *Server) registerMuesliRecordingRoute(api huma.API) {
	op := rawAPIV1Operation("importMuesliRecording", http.MethodPost, "/import/muesli/recording", "Stream retained audio for an archived Muesli meeting")
	for _, name := range []string{"identifier", "account_email", "message_id", "sha256", "size", "no_build_cache"} {
		typ := huma.TypeString
		if name == "message_id" || name == "size" {
			typ = huma.TypeInteger
		}
		if name == "no_build_cache" {
			typ = huma.TypeBoolean
		}
		op.Parameters = append(op.Parameters, &huma.Param{Name: name, In: "query", Required: name != "no_build_cache", Schema: &huma.Schema{Type: typ}})
	}
	op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
		"audio/wav": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
		"audio/mp4": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
	}}
	op.Responses = jsonResponsesFor[muesli.RemoteResult](api, http.StatusOK)
	op.Errors = []int{400, 401, 403, 404, 413, 415, 422, 500, 503}
	registerRawHumaRoute(api, op, s.handleMuesliRecording)
}

func (s *Server) handleMuesliRecording(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "audio/wav" && mediaType != "audio/mp4") {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be audio/wav or audio/mp4")
		return
	}
	query := r.URL.Query()
	messageID, idErr := strconv.ParseInt(query.Get("message_id"), 10, 64)
	size, sizeErr := strconv.ParseInt(query.Get("size"), 10, 64)
	skip := false
	if value := query.Get("no_build_cache"); value != "" {
		skip, err = strconv.ParseBool(value)
	}
	upload := muesli.RecordingUpload{Source: meetingimport.Source{Identifier: query.Get("identifier"), AccountEmail: query.Get("account_email")},
		MessageID: messageID, Size: size, Hash: query.Get("sha256"), MIMEType: mediaType, NoBuildCache: skip}
	var normalizeErr error
	upload, normalizeErr = upload.Normalize()
	if idErr != nil || sizeErr != nil || err != nil || normalizeErr != nil {
		writeError(w, 422, "record_validation_failed", "Recording metadata failed validation")
		return
	}
	capBytes := s.cfg.Server.MuesliRecordingMaxBytes
	if capBytes <= 0 {
		capBytes = muesli.DefaultRecordingMaxBytes
	}
	if size > capBytes || r.ContentLength > capBytes {
		writeError(w, 413, "recording_too_large", muesli.ErrRecordingTooLarge.Error())
		return
	}
	importer, ok := s.store.(MuesliRecordingImporter)
	if !ok || importer == nil {
		writeError(w, 503, "service_unavailable", "Muesli recording import is unavailable")
		return
	}
	staged, err := muesli.StageRecordingUpload(r.Context(), upload, r.Body, capBytes)
	if err != nil {
		s.writeMuesliRecordingImportError(w, err)
		return
	}
	defer func() { _ = staged.Close() }()

	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, "Muesli recording import")
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	changed, err := importer.ImportMuesliRecording(r.Context(), upload, io.NewSectionReader(staged.File, 0, staged.Size), capBytes)
	if err != nil {
		s.writeMuesliRecordingImportError(w, err)
		return
	}
	status := "unchanged"
	if changed {
		status = "updated"
	}
	writeJSON(w, http.StatusOK, muesli.RemoteResult{Status: status, MessageID: messageID, Changed: changed})
}

func (s *Server) writeMuesliRecordingImportError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, muesli.ErrRecordingTooLarge):
		writeError(w, 413, "recording_too_large", muesli.ErrRecordingTooLarge.Error())
	case errors.Is(err, muesli.ErrRemoteValidation), errors.Is(err, muesli.ErrRecordingChanged):
		writeError(w, 422, "record_validation_failed", "Recording failed validation; sync again")
	case errors.Is(err, store.ErrSourceNotFound):
		writeError(w, 404, "source_not_found", "Muesli source is not registered")
	default:
		writeError(w, 500, "internal_error", "Muesli recording import failed")
	}
}
