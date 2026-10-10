package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type InboxCandidateReader interface {
	InboxCandidates(ctx context.Context, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope, limit int, cursor string) (*inboxcontrol.CandidatePage, error)
}

func (s *Server) registerInboxCandidatesRoute(api huma.API) {
	op := rawAPIV1Operation("listInboxCandidates", http.MethodGet, "/inbox/candidates", "List bounded committed inbox metadata for one exact source")
	op.Tags = []string{"Inbox"}
	id := queryIntegerParam("source_id", "Exact archive source ID")
	id.Required = true
	limit := queryIntegerParam("limit", "Maximum candidates (default 25, range 1–100)")
	minimum, maximum := float64(1), float64(100)
	limit.Schema.Minimum = &minimum
	limit.Schema.Maximum = &maximum
	scope := queryStringParam("scope", "Native target scope", true)
	scope.Schema.Enum = []any{"message", "chat"}
	op.Parameters = []*huma.Param{id, queryStringParam("source_type", "Exact provider type", true), queryStringParam("source_identifier", "Exact source identifier", true), queryStringParam("account_id", "Exact provider account", true), scope, limit, queryStringParam("cursor", "Opaque source/scope/archive revision pagination cursor", false)}
	op.Responses = jsonResponsesFor[inboxcontrol.CandidatePage](api)
	for code, response := range jsonResponsesFor[ErrorResponse](api, 400, 401, 403, 409, 500, 503) {
		if code != defaultErrorResponse {
			op.Responses[code] = response
		}
	}
	registerRawHumaRoute(api, op, s.handleInboxCandidates)
}

func (s *Server) handleInboxCandidates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query, err := parseInboxCandidateQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_inbox_request", inboxcontrol.ErrInvalid.Error())
		return
	}
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode == AuthModeRequired {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
		return
	}
	if auth.Mode == AuthModeDelegated && (auth.Grant == nil || !auth.Grant.Allows(agentgrant.PermissionInboxRead, agentgrant.SourceRef{ID: query.source.SourceID, Type: query.source.SourceType, Identifier: query.source.SourceIdentifier})) {
		writeError(w, http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error())
		return
	}
	reader, ok := s.store.(InboxCandidateReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error())
		return
	}
	page, err := reader.InboxCandidates(r.Context(), query.source, query.scope, query.limit, query.cursor)
	if err != nil {
		status, code, message := http.StatusInternalServerError, "inbox_failed", "Unable to read inbox candidates"
		switch {
		case errors.Is(err, inboxcontrol.ErrInvalid):
			status, code, message = http.StatusBadRequest, "invalid_inbox_request", inboxcontrol.ErrInvalid.Error()
		case errors.Is(err, inboxcontrol.ErrDenied):
			status, code, message = http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error()
		case errors.Is(err, inboxcontrol.ErrPlanChanged), errors.Is(err, inboxcontrol.ErrConflict):
			status, code, message = http.StatusConflict, "inbox_conflict", "Inbox pagination conflicts with current evidence"
		case errors.Is(err, inboxcontrol.ErrUnavailable):
			status, code, message = http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error()
		}
		writeError(w, status, code, message)
		return
	}
	if page == nil {
		writeError(w, http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

type inboxCandidateQuery struct {
	source inboxcontrol.SourceIdentity
	scope  inboxcontrol.Scope
	limit  int
	cursor string
}

func parseInboxCandidateQuery(r *http.Request) (inboxCandidateQuery, error) {
	var result inboxCandidateQuery
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return result, inboxcontrol.ErrInvalid
	}
	for name, values := range values {
		switch name {
		case "source_id", "source_type", "source_identifier", "account_id", "scope", "limit", "cursor":
		default:
			return result, inboxcontrol.ErrInvalid
		}
		if len(values) != 1 {
			return result, inboxcontrol.ErrInvalid
		}
	}
	id, err := strconv.ParseInt(values.Get("source_id"), 10, 64)
	if err != nil {
		return result, inboxcontrol.ErrInvalid
	}
	result.source = inboxcontrol.SourceIdentity{SourceID: id, SourceType: values.Get("source_type"), SourceIdentifier: values.Get("source_identifier"), AccountID: values.Get("account_id")}
	result.scope = inboxcontrol.Scope(values.Get("scope"))
	result.limit = 25
	result.cursor = values.Get("cursor")
	if raw, ok := values["limit"]; ok {
		result.limit, err = strconv.Atoi(raw[0])
		if err != nil {
			return result, inboxcontrol.ErrInvalid
		}
	}
	if result.source.Validate() != nil || result.limit < 1 || result.limit > 100 || len(result.cursor) > 16384 || (result.source.SourceType == "beeper" && result.scope != inboxcontrol.ScopeChat) || (result.source.SourceType != "beeper" && result.scope != inboxcontrol.ScopeMessage) {
		return result, inboxcontrol.ErrInvalid
	}
	return result, nil
}
