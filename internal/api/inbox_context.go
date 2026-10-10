package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxContextReader reads one bounded archived body after exact native binding
// checks. It never asks a provider to fetch or mark a message read.
type InboxContextReader interface {
	InboxContext(ctx context.Context, request inboxcontrol.ContextRequest) (*inboxcontrol.Context, error)
}

func (s *Server) registerInboxContextRoute(api huma.API) {
	op := rawAPIV1Operation("getInboxContext", http.MethodPost, "/inbox/context", "Read bounded archived text for one exact inbox target")
	op.Tags = []string{"Inbox"}
	op.RequestBody = jsonRequestBodyFor[inboxcontrol.ContextRequest](api)
	op.Responses = jsonResponsesFor[inboxcontrol.Context](api)
	for code, response := range jsonResponsesFor[ErrorResponse](api, 400, 401, 403, 413, 415, 500, 503) {
		if code != defaultErrorResponse {
			op.Responses[code] = response
		}
	}
	registerRawHumaRoute(api, op, s.handleInboxContext)
}

func (s *Server) handleInboxContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request inboxcontrol.ContextRequest
	if !decodeInboxControlRequest(w, r, &request) {
		return
	}
	if request.Validate() != nil {
		writeError(w, http.StatusBadRequest, "invalid_inbox_request", inboxcontrol.ErrInvalid.Error())
		return
	}
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode == AuthModeRequired {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
		return
	}
	source := agentgrant.SourceRef{ID: request.Target.SourceID, Type: request.Target.SourceType, Identifier: request.Target.SourceIdentifier}
	if auth.Mode == AuthModeDelegated && (auth.Grant == nil || !auth.Grant.Allows(agentgrant.PermissionInboxRead, source) || !auth.Grant.Allows(agentgrant.PermissionInboxContentRead, source)) {
		writeError(w, http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error())
		return
	}
	reader, ok := s.store.(InboxContextReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error())
		return
	}
	result, err := reader.InboxContext(r.Context(), request)
	if err != nil {
		status, code, message := http.StatusInternalServerError, "inbox_failed", "Unable to read archived inbox context"
		switch {
		case errors.Is(err, inboxcontrol.ErrInvalid):
			status, code, message = http.StatusBadRequest, "invalid_inbox_request", inboxcontrol.ErrInvalid.Error()
		case errors.Is(err, inboxcontrol.ErrDenied):
			status, code, message = http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error()
		case errors.Is(err, inboxcontrol.ErrUnavailable):
			status, code, message = http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error()
		}
		writeError(w, status, code, message)
		return
	}
	if result == nil {
		writeError(w, http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
