package api

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/meetingimport"
)

type MeetingImporter interface {
	ImportMeeting(context.Context, meetingimport.Request) (meetingimport.Result, error)
}

type MeetingImportResponse struct {
	Status          meetingimport.Status `json:"status"`
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

func (s *Server) handleMeetingImport(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
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
