package api

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/meetingimport"
)

const meetingImportEndpointPath = "/api/v1/import/meeting"

type MeetingImporter interface {
	ImportMeeting(ctx context.Context, req meetingimport.Request) (meetingimport.Result, error)
}

type MeetingImportResponse struct {
	Status          meetingimport.Status `json:"status" enum:"created,updated"`
	SourceID        int64                `json:"source_id"`
	MessageID       int64                `json:"message_id"`
	SourceMessageID string               `json:"source_message_id"`
}

func (s *Server) registerMeetingImportRoute(api huma.API) {
	op := rawAPIV1Operation(
		"importMeeting",
		http.MethodPost,
		"/import/meeting",
		"Import one meeting",
	)
	op.RequestBody = jsonRequestBodyFor[meetingimport.Request](api)
	hardenMeetingImportSchemas(api.OpenAPI())
	op.Responses = jsonResponsesFor[MeetingImportResponse](
		api,
		http.StatusOK,
		http.StatusCreated,
	)
	op.Errors = []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	}
	registerRawHumaRoute(api, op, s.handleMeetingImport)
}

func hardenMeetingImportSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	meeting := doc.Components.Schemas.Map()["Meeting"]
	if meeting == nil {
		return
	}
	one := 1
	contentRequired := []*huma.Schema{
		{
			Type:     huma.TypeObject,
			Required: []string{"summary_markdown"},
			Properties: map[string]*huma.Schema{
				"summary_markdown": {Type: huma.TypeString, MinLength: &one},
			},
		},
		{
			Type:     huma.TypeObject,
			Required: []string{"summary_text"},
			Properties: map[string]*huma.Schema{
				"summary_text": {Type: huma.TypeString, MinLength: &one},
			},
		},
		{
			Type:     huma.TypeObject,
			Required: []string{"transcript"},
			Properties: map[string]*huma.Schema{
				"transcript": {Type: huma.TypeString, MinLength: &one},
			},
		},
		{
			Type:     huma.TypeObject,
			Required: []string{"transcript_segments"},
			Properties: map[string]*huma.Schema{
				"transcript_segments": {Type: huma.TypeArray, MinItems: &one},
			},
		},
	}
	transcriptsExclusive := &huma.Schema{
		Type:     huma.TypeObject,
		Required: []string{"transcript", "transcript_segments"},
		Properties: map[string]*huma.Schema{
			"transcript":          {Type: huma.TypeString, MinLength: &one},
			"transcript_segments": {Type: huma.TypeArray, MinItems: &one},
		},
	}
	meeting.AllOf = []*huma.Schema{
		{AnyOf: contentRequired},
		{Not: transcriptsExclusive},
	}
}

func (s *Server) handleMeetingImport(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return
	}

	req, err := meetingimport.DecodeRequest(r.Body, meetingimport.MaxRequestBytes)
	switch {
	case errors.Is(err, meetingimport.ErrRequestTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"Meeting import request exceeds 16 MiB")
		return
	case errors.Is(err, meetingimport.ErrMalformedRequest):
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid meeting import JSON")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid meeting import request")
		return
	}
	if _, err := req.Normalize(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed",
			"Meeting import request failed validation")
		return
	}

	importer, ok := s.store.(MeetingImporter)
	if !ok || importer == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"Meeting import is unavailable")
		return
	}

	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(
		gateCtx,
		operationGateLabelFromPath(meetingImportEndpointPath),
	)
	if !ok {
		writeOperationGateBusy(w, s.operationGate)
		return
	}
	defer done()

	result, err := importer.ImportMeeting(r.Context(), req)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		if errors.Is(err, meetingimport.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "service_unavailable",
				"Meeting import is unavailable")
			return
		}
		if errors.Is(err, meetingimport.ErrValidation) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed",
				"Meeting import request failed validation")
			return
		}
		if s.logger != nil {
			s.logger.Error("meeting import failed",
				"source", req.Source.Identifier,
				"external_id", req.Meeting.ExternalID,
				"error_class", "internal")
		}
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Meeting import failed")
		return
	}

	status := http.StatusOK
	if result.Status == meetingimport.StatusCreated {
		status = http.StatusCreated
	}
	writeJSON(w, status, MeetingImportResponse{
		Status:          result.Status,
		SourceID:        result.SourceID,
		MessageID:       result.MessageID,
		SourceMessageID: result.SourceMessageID,
	})
}
