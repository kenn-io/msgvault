package api

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiimport"
)

// importEndpoint is one route that archives a single JSON item, such as a
// meeting or a message. Req is the wire request and Resp the response schema.
type importEndpoint[Req, Resp any] struct {
	operationID string
	path        string // full path, such as /api/v1/import/meeting
	summary     string
	noun        string // capitalized item name for messages, such as "Meeting"
	// hardenSchemas states rules struct tags cannot express.
	hardenSchemas func(*huma.OpenAPI)
	decode        func(*http.Request) (Req, error)
	validate      func(Req) error
	// importer returns the store's importer, or false when it has none.
	importer func(*Server) (func(context.Context, Req) (apiimport.Result, error), bool)
	// logFields identify a failed request in the server log.
	logFields func(Req) []any
	respond   func(apiimport.Result) Resp
}

func registerImportRoute[Req, Resp any](s *Server, api huma.API, e importEndpoint[Req, Resp]) {
	op := rawAPIV1Operation(e.operationID, http.MethodPost, strings.TrimPrefix(e.path, "/api/v1"), e.summary)
	op.RequestBody = jsonRequestBodyFor[Req](api)
	e.hardenSchemas(api.OpenAPI())
	op.Responses = jsonResponsesFor[Resp](api, http.StatusOK, http.StatusCreated)
	op.Errors = []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	}
	registerRawHumaRoute(api, op, func(w http.ResponseWriter, r *http.Request) {
		handleImport(s, w, r, e)
	})
}

func handleImport[Req, Resp any](s *Server, w http.ResponseWriter, r *http.Request, e importEndpoint[Req, Resp]) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return
	}

	req, err := e.decode(r)
	switch {
	case errors.Is(err, apiimport.ErrRequestTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			e.noun+" import request exceeds 16 MiB")
		return
	case errors.Is(err, apiimport.ErrMalformedRequest):
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid "+strings.ToLower(e.noun)+" import JSON")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid "+strings.ToLower(e.noun)+" import request")
		return
	}
	if err := e.validate(req); err != nil {
		// Validation messages name the field and the rule, never the value.
		message := e.noun + " import request failed validation"
		if detail, ok := apiimport.ValidationDetail(err); ok {
			message += ": " + detail
		}
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", message)
		return
	}

	importItem, ok := e.importer(s)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			e.noun+" import is unavailable")
		return
	}

	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(
		gateCtx,
		operationGateLabelFromPath(e.path),
	)
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()

	result, err := importItem(r.Context(), req)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		if errors.Is(err, apiimport.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "service_unavailable",
				e.noun+" import is unavailable")
			return
		}
		if s.logger != nil {
			s.logger.Error(strings.ToLower(e.noun)+" import failed",
				append(e.logFields(req), "error_class", "internal")...)
		}
		writeError(w, http.StatusInternalServerError, "internal_error",
			e.noun+" import failed")
		return
	}

	status := http.StatusOK
	if result.Status == apiimport.StatusCreated {
		status = http.StatusCreated
	}
	writeJSON(w, status, e.respond(result))
}
