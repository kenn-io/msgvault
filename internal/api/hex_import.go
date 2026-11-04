package api

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/store"
)

const hexImportEndpointPath = "/api/v1/import/hex"

type HexImporter interface {
	ImportHex(ctx context.Context, req hex.RemoteRequest) (hex.RemoteResult, error)
}

func (s *Server) registerHexImportRoute(api huma.API) {
	s.registerHexRecordingRoute(api)
	op := rawAPIV1Operation("importHex", http.MethodPost, "/import/hex", "Import one recorder-local Hex meeting")
	op.RequestBody = jsonRequestBodyFor[hex.RemoteRequest](api)
	op.Responses = jsonResponsesFor[hex.RemoteResult](api, http.StatusOK, http.StatusCreated)
	op.Errors = []int{400, 401, 404, 413, 415, 422, 500, 503}
	registerRawHumaRoute(api, op, s.handleHexImport)
}
func (s *Server) handleHexImport(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != applicationJSONMediaType {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	req, err := hex.DecodeRemoteRequest(r.Body, hex.MaxRemoteRequestBytes)
	switch {
	case errors.Is(err, hex.ErrRemoteTooLarge):
		writeError(w, 413, "request_too_large", "Hex transfer exceeds 16 MiB")
		return
	case errors.Is(err, hex.ErrRemoteMalformed):
		writeError(w, 400, "bad_request", "Invalid Hex transfer JSON")
		return
	case err != nil:
		writeError(w, 422, "validation_failed", "Hex transfer failed validation")
		return
	}
	importer, ok := s.store.(HexImporter)
	if !ok || importer == nil {
		writeError(w, 503, "service_unavailable", "Hex import is unavailable")
		return
	}
	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, operationGateLabelFromPath(hexImportEndpointPath))
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	result, err := importer.ImportHex(r.Context(), req)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		switch {
		case errors.Is(err, hex.ErrRemoteRecordValidation):
			writeError(w, 422, "record_validation_failed", "Hex meeting failed validation")
		case errors.Is(err, hex.ErrRemoteValidation):
			writeError(w, 422, "validation_failed", "Hex transfer failed validation")
		case errors.Is(err, store.ErrSourceNotFound):
			writeError(w, 404, "source_not_found", "Hex source is not registered; run msgvault add-hex first")
		default:
			if s.logger != nil {
				s.logger.Error("Hex import failed", "error_class", "internal")
			}
			writeError(w, 500, "internal_error", "Hex import failed")
		}
		return
	}
	status := http.StatusOK
	if result.Status == "created" {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
