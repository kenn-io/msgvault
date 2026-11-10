package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/emailtags"
)

// MessageArchiveStore authorizes the exact source and sender before provider access.
type MessageArchiveStore interface {
	ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange, grant *agentgrant.Grant, acquire func(context.Context) (func(), error)) (*emailtags.ArchiveResult, error)
}

func (s *Server) registerMessageArchiveRoute(api huma.API) {
	op := rawAPIV1Operation("archiveMessage", http.MethodPost, "/messages/{id}/archive", "Set a Gmail message's archive state")
	op.Parameters = []*huma.Param{param("id", "path", huma.TypeInteger, "Archived message ID", true)}
	op.RequestBody = jsonRequestBodyFor[emailtags.ArchiveChange](api)
	op.Responses = jsonResponsesFor[emailtags.ArchiveResult](api)
	for key, response := range jsonResponsesFor[emailtags.MessageTagError](api, 400, 403, 404, 409, 500, 501, 502, 503) {
		if key != "default" {
			op.Responses[key] = response
		}
	}
	registerRawHumaRoute(api, op, s.handleMessageArchive)
}

func (s *Server) handleMessageArchive(w http.ResponseWriter, r *http.Request) {
	auth := s.requestAuthentication(r)
	if !s.apiRequestAuthorized(r) && auth.Grant == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Owner or delegated authorization is required")
		return
	}
	if auth.Grant != nil && !auth.Grant.HasPermission(agentgrant.PermissionMessageArchive) {
		writeError(w, http.StatusForbidden, "permission_denied", "Agent grant requires message.archive")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "message ID must be a positive integer")
		return
	}
	var change emailtags.ArchiveChange
	if !decodeEntityRequest(w, r, &change, "message archive") {
		return
	}
	if change.Archived == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "archived must be true or false")
		return
	}
	backend, ok := s.store.(MessageArchiveStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "archive_unavailable", "Provider archiving is unavailable")
		return
	}
	result, err := backend.ArchiveMessage(r.Context(), id, change, auth.Grant, s.beginMutation("message archive change"))
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		if errors.Is(err, errMutationGateBusy) {
			writeOperationGateBusy(w, r, s.operationGate)
			return
		}
		// Archive authority covers the INBOX toggle, not unrelated label state.
		// The backend already checks this message's source against the grant.
		if auth.Grant != nil && !auth.Grant.HasPermission(agentgrant.PermissionMessageRead) {
			if failure, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
				redacted := *failure
				redacted.Result = nil
				err = &redacted
			}
		}
		s.writeEmailTagError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
