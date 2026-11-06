package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/muesli"
)

const muesliImportEndpointPath = "/api/v1/import/muesli"

type MuesliImporter interface {
	ImportMuesli(ctx context.Context, req muesli.RemoteRequest) (muesli.RemoteResult, error)
}

func (s *Server) registerMuesliImportRoute(api huma.API) {
	s.registerMuesliRecordingRoute(api)
	op := rawAPIV1Operation("importMuesli", http.MethodPost, "/import/muesli", "Import one recorder-local Muesli meeting")
	op.RequestBody = jsonRequestBodyFor[muesli.RemoteRequest](api)
	op.Responses = jsonResponsesFor[muesli.RemoteResult](api, http.StatusOK, http.StatusCreated)
	op.Errors = []int{400, 401, 404, 413, 415, 422, 500, 503}
	registerRawHumaRoute(api, op, s.handleMuesliImport)
}

var muesliImportErrors = recorderImportErrors{"Muesli", muesli.ErrRemoteMalformed, muesli.ErrRemoteTooLarge, muesli.ErrRemoteValidation, muesli.ErrRemoteRecordValidation}

func (s *Server) handleMuesliImport(w http.ResponseWriter, r *http.Request) {
	var run func(context.Context, muesli.RemoteRequest) (muesli.RemoteResult, error)
	if importer, ok := s.store.(MuesliImporter); ok && importer != nil {
		run = importer.ImportMuesli
	}
	serveRecorderImport(s, w, r, muesliImportEndpointPath, muesliImportErrors, muesli.DecodeRemoteRequest, run, func(result muesli.RemoteResult) bool { return result.Status == "created" })
}
