package api

import (
	"context"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/hex"
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
	var run func(context.Context, hex.RecordingUpload, io.Reader, int64) (bool, error)
	if importer, ok := s.store.(HexRecordingImporter); ok && importer != nil {
		run = importer.ImportHexRecording
	}
	serveRecorderRecording(s, w, r, hexImportErrors, s.cfg.Server.HexRecordingMaxBytes, []string{"audio/wav"}, func(c recorderRecordingClaims) (hex.RecordingUpload, error) {
		return (hex.RecordingUpload{Source: c.Source, MessageID: c.MessageID, Size: c.Size, Hash: c.Hash, MIMEType: c.MIMEType, NoBuildCache: c.NoBuildCache, Track: c.Track}).Normalize()
	}, hex.StageRecordingUpload, run)
}
