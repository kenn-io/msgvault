package api

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/messageimport"
)

const messageImportEndpointPath = "/api/v1/import/message"

type MessageImporter interface {
	ImportMessage(ctx context.Context, req messageimport.Request) (messageimport.Result, error)
}

type MessageImportResponse struct {
	Status          messageimport.Status `json:"status" enum:"created,updated"`
	SourceID        int64                `json:"source_id"`
	MessageID       int64                `json:"message_id"`
	SourceMessageID string               `json:"source_message_id"`
}

func (s *Server) registerMessageImportRoute(api huma.API) {
	op := rawAPIV1Operation(
		"importMessage",
		http.MethodPost,
		"/import/message",
		"Import one message",
	)
	op.RequestBody = jsonRequestBodyFor[messageimport.Request](api)
	hardenMessageImportSchemas(api.OpenAPI())
	op.Responses = jsonResponsesFor[MessageImportResponse](
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
	registerRawHumaRoute(api, op, s.handleMessageImport)
}

// hardenMessageImportSchemas states the cross-field rules that struct tags
// cannot: a sender needs one identifying field, a message needs a title or a
// body, and the body fields are mutually exclusive.
func hardenMessageImportSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schemas := doc.Components.Schemas.Map()
	one := 1
	nonEmpty := func(field string) *huma.Schema {
		return &huma.Schema{
			Type:       huma.TypeObject,
			Required:   []string{field},
			Properties: map[string]*huma.Schema{field: {Type: huma.TypeString, MinLength: &one}},
		}
	}
	bothNonEmpty := func(left, right string) *huma.Schema {
		return &huma.Schema{
			Type:     huma.TypeObject,
			Required: []string{left, right},
			Properties: map[string]*huma.Schema{
				left:  {Type: huma.TypeString, MinLength: &one},
				right: {Type: huma.TypeString, MinLength: &one},
			},
		}
	}

	if sender := schemas["MessageImportSender"]; sender != nil {
		phoneLimit := 64
		if phone := sender.Properties["phone"]; phone != nil {
			phone.MaxLength = &phoneLimit
		}
		sender.AnyOf = []*huma.Schema{
			nonEmpty("name"), nonEmpty("url"), nonEmpty("email"), nonEmpty("phone"), nonEmpty("id"),
		}
	}
	message := schemas["ImportedMessage"]
	if message == nil {
		return
	}
	message.AllOf = []*huma.Schema{
		{AnyOf: []*huma.Schema{
			nonEmpty("title"), nonEmpty("body_markdown"), nonEmpty("body_html"), nonEmpty("body_text"),
		}},
		{Not: &huma.Schema{AnyOf: []*huma.Schema{
			bothNonEmpty("body_markdown", "body_html"),
			bothNonEmpty("body_markdown", "body_text"),
			bothNonEmpty("body_html", "body_text"),
		}}},
	}
}

func (s *Server) handleMessageImport(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return
	}

	req, err := messageimport.DecodeRequest(r.Body, messageimport.MaxRequestBytes)
	switch {
	case errors.Is(err, messageimport.ErrRequestTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"Message import request exceeds 16 MiB")
		return
	case errors.Is(err, messageimport.ErrMalformedRequest):
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid message import JSON")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid message import request")
		return
	}
	if _, err := req.Normalize(); err != nil {
		// Validation messages name the field and the rule, never the value.
		message := "Message import request failed validation"
		if detail, ok := strings.CutPrefix(err.Error(), messageimport.ErrValidation.Error()+": "); ok &&
			errors.Is(err, messageimport.ErrValidation) {
			message += ": " + detail
		}
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", message)
		return
	}

	importer, ok := s.store.(MessageImporter)
	if !ok || importer == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"Message import is unavailable")
		return
	}

	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(
		gateCtx,
		operationGateLabelFromPath(messageImportEndpointPath),
	)
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()

	result, err := importer.ImportMessage(r.Context(), req)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		if errors.Is(err, messageimport.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "service_unavailable",
				"Message import is unavailable")
			return
		}
		if errors.Is(err, messageimport.ErrValidation) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed",
				"Message import request failed validation")
			return
		}
		if s.logger != nil {
			s.logger.Error("message import failed",
				"source", req.Source.Identifier,
				"external_id", req.Message.ExternalID,
				"error_class", "internal")
		}
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Message import failed")
		return
	}

	status := http.StatusOK
	if result.Status == messageimport.StatusCreated {
		status = http.StatusCreated
	}
	writeJSON(w, status, MessageImportResponse{
		Status:          result.Status,
		SourceID:        result.SourceID,
		MessageID:       result.MessageID,
		SourceMessageID: result.SourceMessageID,
	})
}
