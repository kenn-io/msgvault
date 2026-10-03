package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/emailtags"
)

// MessageTagStore edits native provider tags on an archived message.
type MessageTagStore interface {
	MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error)
}

func (s *Server) registerEmailTagRoutes(api huma.API) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		id, summary := "getMessageTags", "Read native email tags"
		if method == http.MethodPost {
			id, summary = "updateMessageTags", "Add or remove native email tags"
		}
		op := rawAPIV1Operation(id, method, "/messages/{id}/tags", summary)
		op.Parameters = []*huma.Param{param("id", "path", huma.TypeInteger, "Archived message ID", true)}
		if method == http.MethodPost {
			op.RequestBody = jsonRequestBodyFor[emailtags.Change](api)
		} else {
			op.Parameters = append(op.Parameters, queryStringParam("mailbox", "Exact IMAP mailbox; defaults to the archived primary membership", false))
		}
		op.Responses = jsonResponsesFor[emailtags.Result](api)
		for key, response := range jsonResponsesFor[emailtags.Error](api, 400, 403, 404, 409, 500, 501, 502, 503) {
			if key != "default" {
				op.Responses[key] = response
			}
		}
		registerRawHumaRoute(api, op, s.handleMessageTags)
	}
}

func (s *Server) handleMessageTags(w http.ResponseWriter, r *http.Request) {
	if !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Owner authorization is required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "message ID must be a positive integer")
		return
	}
	backend, ok := s.store.(MessageTagStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "tags_unavailable", "Provider tag editing is unavailable")
		return
	}
	mailbox := r.URL.Query().Get("mailbox")
	var change *emailtags.Change
	if r.Method == http.MethodPost {
		change = &emailtags.Change{}
		if !decodeEntityRequest(w, r, change, "message tags") {
			return
		}
		normalized, err := emailtags.Normalize(*change, false)
		if err != nil {
			s.writeEmailTagError(w, err)
			return
		}
		*change = normalized
		mailbox = change.Mailbox
	}
	result, err := backend.MessageTags(r.Context(), id, change, mailbox)
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		s.writeEmailTagError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) writeEmailTagError(w http.ResponseWriter, err error) {
	var failure *emailtags.Error
	if !errors.As(err, &failure) {
		writeError(w, http.StatusInternalServerError, "tags_failed", "Unable to access provider tags")
		return
	}
	status := http.StatusBadGateway
	switch failure.Code {
	case "invalid_tag", "invalid_request", "unavailable_tag":
		status = http.StatusBadRequest
	case "message_not_found":
		status = http.StatusNotFound
	case "insufficient_scope", "keywords_not_permitted":
		status = http.StatusForbidden
	case "stale_identity", "sync_active":
		status = http.StatusConflict
	case "unsupported_provider", "unsupported_keywords":
		status = http.StatusNotImplemented
	case "remote_accepted_local_failed", "local_failed":
		status = http.StatusInternalServerError
	case "provider_unavailable":
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, failure)
}
