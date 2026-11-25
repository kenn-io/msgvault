package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/hex"
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

var hexImportErrors = recorderImportErrors{"Hex", hex.ErrRemoteMalformed, hex.ErrRemoteTooLarge, hex.ErrRemoteValidation, hex.ErrRemoteRecordValidation}

func (s *Server) handleHexImport(w http.ResponseWriter, r *http.Request) {
	var run func(context.Context, hex.RemoteRequest) (hex.RemoteResult, error)
	if importer, ok := s.store.(HexImporter); ok && importer != nil {
		run = importer.ImportHex
	}
	serveRecorderImport(s, w, r, hexImportEndpointPath, hexImportErrors, hex.DecodeRemoteRequest, run, func(result hex.RemoteResult) bool { return result.Status == "created" })
}
