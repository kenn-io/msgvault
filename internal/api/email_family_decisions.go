package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

type EmailFamilyDecisionStore interface {
	GetEmailFamilyDecisionContext(ctx context.Context, decisionID int64, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error)
	PreviewEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionPreview, error)
	ApplyEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionResult, error)
	UndoEmailFamilyDecisionContext(ctx context.Context, decisionID int64, revision int64, familyRevision string, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error)
	ListEmailFamilyDecisionsContext(ctx context.Context, messageID int64, options store.EmailFamilyOptions) ([]store.EmailFamilyDecision, error)
}

type EmailFamilyDecisionUndo struct {
	ExpectedRevision       int64   `json:"expected_revision"`
	ExpectedFamilyRevision string  `json:"expected_family_revision"`
	SourceIDs              []int64 `json:"source_ids,omitzero"`
}

func (s *Server) registerEmailFamilyDecisionRoutes(api huma.API) {
	for _, preview := range []bool{true, false} {
		path, id, summary := "/messages/family/decisions", "applyEmailFamilyDecision", "Apply a reversible local email family decision"
		if preview {
			path += "/preview"
			id = "previewEmailFamilyDecision"
			summary = "Preview local links or exact-copy handling without saving"
		}
		op := rawAPIV1Operation(id, http.MethodPost, path, summary)
		op.RequestBody = jsonRequestBodyFor[store.EmailFamilyDecisionInput](api)
		if preview {
			op.Responses = jsonResponsesFor[store.EmailFamilyDecisionPreview](api)
		} else {
			op.Responses = jsonResponsesFor[store.EmailFamilyDecisionResult](api)
		}
		addErrorResponses(api, op.Responses, 400, 401, 403, 404, 409, 503)
		registerRawHumaRoute(api, op, s.handleEmailFamilyDecision)
	}
	op := rawAPIV1Operation("undoEmailFamilyDecision", http.MethodPost, "/messages/family/decisions/{id}/undo", "Undo one local family decision with revision checks")
	op.Parameters = []*huma.Param{pathIntegerParam("Decision ID")}
	op.RequestBody = jsonRequestBodyFor[EmailFamilyDecisionUndo](api)
	op.Responses = jsonResponsesFor[store.EmailFamilyDecisionResult](api)
	addErrorResponses(api, op.Responses, 400, 401, 403, 404, 409, 503)
	registerRawHumaRoute(api, op, s.handleUndoEmailFamilyDecision)
	op = rawAPIV1Operation("getEmailFamilyDecision", http.MethodGet, "/messages/family/decisions/{id}", "Read a local decision and its current surviving copy union")
	op.Parameters = []*huma.Param{pathIntegerParam("Decision ID"), queryIntegerArrayParam("source_id", "Narrow the source set")}
	op.Responses = jsonResponsesFor[store.EmailFamilyDecisionResult](api)
	addErrorResponses(api, op.Responses, 400, 401, 403, 404, 503)
	registerRawHumaRoute(api, op, s.handleGetEmailFamilyDecision)
	op = rawAPIV1Operation("listEmailFamilyDecisions", http.MethodGet, "/messages/family/decisions", "Read the latest 20 local decisions for one live copy")
	op.Parameters = []*huma.Param{queryIntegerParam("message_id", "Live archived message ID"), queryIntegerArrayParam("source_id", "Narrow the source set")}
	op.Responses = jsonResponsesFor[[]store.EmailFamilyDecision](api)
	addErrorResponses(api, op.Responses, 400, 401, 403, 404, 503)
	registerRawHumaRoute(api, op, s.handleListEmailFamilyDecisions)
}

func (s *Server) emailFamilyDecisionBackend(w http.ResponseWriter, r *http.Request) (EmailFamilyDecisionStore, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.apiRequestAuthorized(r) {
		writeError(w, 401, "unauthorized", "Owner authorization is required")
		return nil, false
	}
	backend, ok := s.store.(EmailFamilyDecisionStore)
	if !ok {
		writeError(w, 503, "email_family_unavailable", "Email family decisions are unavailable")
	}
	return backend, ok
}

func (s *Server) handleEmailFamilyDecision(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.emailFamilyDecisionBackend(w, r)
	if !ok {
		return
	}
	var input store.EmailFamilyDecisionInput
	if !decodeEntityRequest(w, r, &input, "email family decision") {
		return
	}
	if r.URL.Path == "/api/v1/messages/family/decisions/preview" {
		result, err := backend.PreviewEmailFamilyDecisionContext(r.Context(), input)
		if err != nil {
			s.writeEmailFamilyDecisionError(w, err)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	done, err := s.beginMutation("email family decision")(r.Context())
	if err != nil {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	result, err := backend.ApplyEmailFamilyDecisionContext(r.Context(), input)
	if err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleUndoEmailFamilyDecision(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.emailFamilyDecisionBackend(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "invalid_request", "Decision ID must be positive")
		return
	}
	var input EmailFamilyDecisionUndo
	if !decodeEntityRequest(w, r, &input, "email family undo") {
		return
	}
	done, err := s.beginMutation("undo email family decision")(r.Context())
	if err != nil {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	result, err := backend.UndoEmailFamilyDecisionContext(r.Context(), id, input.ExpectedRevision, input.ExpectedFamilyRevision, store.EmailFamilyOptions{SourceIDs: input.SourceIDs})
	if err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleGetEmailFamilyDecision(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.emailFamilyDecisionBackend(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "invalid_request", "Decision ID must be positive")
		return
	}
	options, err := emailFamilyOptions(r)
	if err != nil {
		writeError(w, 400, "invalid_source_ids", err.Error())
		return
	}
	result, err := backend.GetEmailFamilyDecisionContext(r.Context(), id, options)
	if err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleListEmailFamilyDecisions(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.emailFamilyDecisionBackend(w, r)
	if !ok {
		return
	}
	id, err := contactQueryInteger(r, "message_id", 1, 0)
	if err != nil || id < 1 {
		writeError(w, 400, "invalid_request", "message_id must be positive")
		return
	}
	options, err := emailFamilyOptions(r)
	if err != nil {
		writeError(w, 400, "invalid_source_ids", err.Error())
		return
	}
	result, err := backend.ListEmailFamilyDecisionsContext(r.Context(), id, options)
	if err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) writeEmailFamilyDecisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "not_found", "Message or decision not found")
	case errors.Is(err, store.ErrEmailFamilyChanged):
		writeError(w, 409, "family_changed", "Family evidence changed; preview again")
	case errors.Is(err, store.ErrEmailFamilyRequestConflict):
		writeError(w, 409, "request_conflict", "Request key already identifies different content")
	case errors.Is(err, store.ErrEmailFamilyTruncated):
		writeError(w, 409, "family_truncated", "A truncated family cannot be changed")
	case errors.Is(err, store.ErrEmailFamilyDecisionInvalid):
		writeError(w, 400, "invalid_request", err.Error())
	default:
		s.logger.Error("email family decision failed", "error", err)
		writeError(w, 503, "email_family_unavailable", "Email family decisions are unavailable")
	}
}
