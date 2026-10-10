package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxTriageController uses the shared native service and current authorization.
// Preview reads only; apply owns one daemon gate and exact source lease and
// preserves ordered durable receipts when any item fails.
type InboxTriageController interface {
	PreviewInboxTriage(ctx context.Context, input inboxcontrol.TriageInput, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) (*inboxcontrol.TriageProposal, error)
	ApplyInboxTriage(ctx context.Context, proposal inboxcontrol.TriageProposal, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) ([]inboxcontrol.Result, error)
}

type InboxTriageError struct {
	Error   string                `json:"error"`
	Message string                `json:"message"`
	Results []inboxcontrol.Result `json:"results,omitempty"`
}

func (s *Server) registerInboxTriageRoutes(api huma.API) {
	preview := rawAPIV1Operation("previewInboxTriage", http.MethodPost, "/inbox/triage/preview", "Preview a bounded tag-only triage proposal for explicit inbox targets")
	preview.Tags = []string{"Inbox"}
	preview.RequestBody = jsonRequestBodyFor[inboxcontrol.TriageInput](api)
	preview.Responses = jsonResponsesFor[inboxcontrol.TriageProposal](api)
	apply := rawAPIV1Operation("applyInboxTriage", http.MethodPost, "/inbox/triage/apply", "Apply one authenticated triage proposal with per-item durable receipts")
	apply.Tags = []string{"Inbox"}
	apply.RequestBody = jsonRequestBodyFor[inboxcontrol.TriageProposal](api)
	apply.Responses = jsonResponsesFor[[]inboxcontrol.Result](api)
	for _, operation := range []*huma.Operation{&preview, &apply} {
		for code, response := range jsonResponsesFor[InboxTriageError](api, 400, 401, 403, 409, 413, 415, 500, 502, 503) {
			if code != defaultErrorResponse {
				operation.Responses[code] = response
			}
		}
	}
	registerRawHumaRoute(api, preview, s.handlePreviewInboxTriage)
	registerRawHumaRoute(api, apply, s.handleApplyInboxTriage)
}

func (s *Server) handlePreviewInboxTriage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input inboxcontrol.TriageInput
	if !decodeInboxControlRequest(w, r, &input) {
		return
	}
	if err := input.Validate(); err != nil {
		writeInboxTriageError(w, err, nil)
		return
	}
	p, authorize, err := s.inboxAuthorization(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
		return
	}
	backend, ok := s.store.(InboxTriageController)
	if !ok {
		writeInboxTriageError(w, inboxcontrol.ErrUnavailable, nil)
		return
	}
	proposal, err := backend.PreviewInboxTriage(r.Context(), input, p, authorize)
	if err != nil {
		writeInboxTriageError(w, err, nil)
		return
	}
	if proposal == nil {
		writeInboxTriageError(w, inboxcontrol.ErrInternal, nil)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (s *Server) handleApplyInboxTriage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var proposal inboxcontrol.TriageProposal
	if !decodeInboxControlRequest(w, r, &proposal) {
		return
	}
	p, authorize, err := s.inboxAuthorization(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
		return
	}
	backend, ok := s.store.(InboxTriageController)
	if !ok {
		writeInboxTriageError(w, inboxcontrol.ErrUnavailable, nil)
		return
	}
	results, err := backend.ApplyInboxTriage(r.Context(), proposal, p, authorize, s.beginInboxMutation)
	if err != nil {
		writeInboxTriageError(w, err, results)
		return
	}
	if len(results) != len(proposal.Items) || len(results) == 0 {
		writeInboxTriageError(w, inboxcontrol.ErrInternal, results)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func writeInboxTriageError(w http.ResponseWriter, err error, results []inboxcontrol.Result) {
	status, code, message := inboxControlErrorStatus(err)
	if errors.Is(err, errCalendarGateBusy) {
		status, code, message = http.StatusServiceUnavailable, "operation_in_progress", "Inbox execution is busy or shutting down"
	}
	writeJSON(w, status, InboxTriageError{Error: code, Message: message, Results: results})
}
