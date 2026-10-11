package api

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
)

const muesliImportEndpointPath = "/api/v1/import/muesli"

type MuesliImporter interface {
	ImportMuesli(ctx context.Context, req muesli.RemoteRequest) (muesli.RemoteResult, error)
}

func (s *Server) registerMuesliImportRoute(api huma.API) {
	op := rawAPIV1Operation("importMuesli", http.MethodPost, "/import/muesli", "Import one recorder-local Muesli meeting")
	op.RequestBody = jsonRequestBodyFor[muesli.RemoteRequest](api)
	op.Responses = jsonResponsesFor[muesli.RemoteResult](api, http.StatusOK, http.StatusCreated)
	op.Errors = []int{400, 401, 404, 413, 415, 422, 500, 503}
	registerRawHumaRoute(api, op, s.handleMuesliImport)
}
func (s *Server) handleMuesliImport(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != applicationJSONMediaType {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	req, err := muesli.DecodeRemoteRequest(r.Body, muesli.MaxRemoteRequestBytes)
	switch {
	case errors.Is(err, muesli.ErrRemoteTooLarge):
		writeError(w, 413, "request_too_large", "Muesli transfer exceeds 16 MiB")
		return
	case errors.Is(err, muesli.ErrRemoteMalformed):
		writeError(w, 400, "bad_request", "Invalid Muesli transfer JSON")
		return
	case err != nil:
		writeError(w, 422, "validation_failed", "Muesli transfer failed validation")
		return
	}
	importer, ok := s.store.(MuesliImporter)
	if !ok || importer == nil {
		writeError(w, 503, "service_unavailable", "Muesli import is unavailable")
		return
	}
	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, operationGateLabelFromPath(muesliImportEndpointPath))
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	result, err := importer.ImportMuesli(r.Context(), req)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		switch {
		case errors.Is(err, muesli.ErrRemoteRecordValidation):
			writeError(w, 422, "record_validation_failed", "Muesli meeting failed validation")
		case errors.Is(err, muesli.ErrRemoteValidation):
			writeError(w, 422, "validation_failed", "Muesli transfer failed validation")
		case errors.Is(err, store.ErrSourceNotFound):
			writeError(w, 404, "source_not_found", "Muesli source is not registered; run msgvault add-muesli first")
		default:
			if s.logger != nil {
				s.logger.Error("Muesli import failed", "error_class", "internal")
			}
			writeError(w, 500, "internal_error", "Muesli import failed")
		}
		return
	}
	status := http.StatusOK
	if result.Status == "created" {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
