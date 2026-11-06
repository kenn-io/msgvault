package api

import (
	"context"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/muesli"
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
	var run func(context.Context, muesli.RecordingUpload, io.Reader, int64) (bool, error)
	if importer, ok := s.store.(MuesliRecordingImporter); ok && importer != nil {
		run = importer.ImportMuesliRecording
	}
	serveRecorderRecording(s, w, r, muesliImportErrors, s.cfg.Server.MuesliRecordingMaxBytes, []string{"audio/wav", "audio/mp4"}, func(c recorderRecordingClaims) (muesli.RecordingUpload, error) {
		return (muesli.RecordingUpload{Source: c.Source, MessageID: c.MessageID, Size: c.Size, Hash: c.Hash, MIMEType: c.MIMEType, NoBuildCache: c.NoBuildCache}).Normalize()
	}, muesli.StageRecordingUpload, run)
}
