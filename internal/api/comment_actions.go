package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/meta"
)

// MetaCommentDisclosure gives comment action disclosures a distinct OpenAPI name.
type MetaCommentDisclosure meta.Disclosure

type CommentActionStore interface {
	RunCommentAction(ctx context.Context, id int64, req meta.ActionRequest, acquireWrite func(context.Context) (func(), error)) (*meta.ActionResult, error)
	ListCommentActions(ctx context.Context, id int64) ([]meta.Receipt, error)
	UpdateCommentTriage(ctx context.Context, id int64, state, expect string, acquireWrite func(context.Context) (func(), error)) (string, error)
	ListCommentQueue(ctx context.Context, req CommentQueueRequest) (*CommentQueueResponse, error)
}
type CommentActionBody struct {
	Action         meta.ActionKind `json:"action" enum:"hide,unhide,delete"`
	Body           string          `json:"body,omitempty"`
	Confirm        string          `json:"confirm,omitempty"`
	ResolveUnknown string          `json:"resolve_unknown,omitempty" enum:"sent,not-sent"`
	DryRun         bool            `json:"dry_run,omitempty"`
}
type CommentTriageBody struct {
	State  string `json:"state" enum:"open,handled,skipped"`
	Expect string `json:"expect,omitempty" enum:"open,handled,skipped"`
}
type CommentTriageResponse struct {
	State string `json:"state"`
}
type CommentQueueRequest = meta.CommentQueueRequest
type CommentQueueAuthor = meta.CommentQueueAuthor
type CommentQueueItem = meta.CommentQueueItem
type CommentQueueResponse = meta.CommentQueueResponse

func commentActionOwnerOnly(id string) bool {
	switch id {
	case "runCommentAction", "listCommentActions", "updateCommentTriage", "listCommentQueue":
		return true
	}
	return false
}
func commentTriageRouteRequest(r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/messages/"), "/")
	return r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/messages/") && len(parts) == 2 && parts[0] != "" && parts[1] == "comment-triage"
}
func (s *Server) registerCommentActionRoutes(api huma.API) {
	for _, route := range []struct{ id, method, path, summary string }{
		{"runCommentAction", "POST", "/messages/{id}/comment-actions", "Preview or perform a comment action"},
		{"listCommentActions", "GET", "/messages/{id}/comment-actions", "Read comment action receipts"},
		{"updateCommentTriage", "PUT", "/messages/{id}/comment-triage", "Set local comment triage state"},
		{"listCommentQueue", "GET", "/comment-queue", "List archived comments by triage state"},
	} {
		op := rawAPIV1Operation(route.id, route.method, route.path, route.summary)
		if route.id != "listCommentQueue" {
			op.Parameters = []*huma.Param{param("id", "path", huma.TypeInteger, "Archived comment message ID", true)}
		}
		switch route.id {
		case "runCommentAction":
			op.RequestBody = jsonRequestBodyFor[CommentActionBody](api)
			op.Responses = jsonResponsesFor[meta.ActionResult](api)
		case "listCommentActions":
			op.Responses = jsonResponsesFor[[]meta.Receipt](api)
		case "updateCommentTriage":
			op.RequestBody = jsonRequestBodyFor[CommentTriageBody](api)
			op.Responses = jsonResponsesFor[CommentTriageResponse](api)
		case "listCommentQueue":
			op.Parameters = []*huma.Param{queryStringParam("source", "Exact archive source identifier", false), queryStringParam("state", "open (default), handled, skipped or all", false), queryIntegerParam("limit", "Rows per page, 1–200 (default 50)"), queryIntegerParam("before_id", "Last-seen message ID in ascending sent_at/id order")}
			op.Responses = jsonResponsesFor[CommentQueueResponse](api)
		}
		for code, response := range jsonResponsesFor[meta.ActionError](api, 400, 401, 403, 404, 409, 429, 500, 502, 503) {
			if code != "default" {
				op.Responses[code] = response
			}
		}
		registerRawHumaRoute(api, op, s.handleCommentActions)
	}
}
func ValidateCommentQueueRequest(f CommentQueueRequest) error {
	return meta.ValidateCommentQueueRequest(f)
}

func (s *Server) handleCommentActions(w http.ResponseWriter, r *http.Request) {
	if !s.apiRequestAuthorized(r) {
		writeError(w, 401, "unauthorized", "Owner authorization is required")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	backend, ok := s.store.(CommentActionStore)
	if !ok {
		writeError(w, 503, "actions_unavailable", "Comment actions are unavailable")
		return
	}
	var value any
	var err error
	if r.URL.Path == "/api/v1/comment-queue" {
		q := r.URL.Query()
		f := CommentQueueRequest{Source: q.Get("source"), State: q.Get("state")}
		if q.Has("limit") {
			f.Limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil || f.Limit == 0 {
				writeError(w, 400, "invalid_request", "limit must be 1–200")
				return
			}
		}
		if q.Has("before_id") {
			f.BeforeID, err = strconv.ParseInt(q.Get("before_id"), 10, 64)
			if err != nil {
				writeError(w, 400, "invalid_request", "before_id must be a non-negative integer")
				return
			}
		}
		if err = ValidateCommentQueueRequest(f); err == nil {
			value, err = backend.ListCommentQueue(r.Context(), f)
		}
	} else {
		id, parseErr := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if parseErr != nil || id <= 0 {
			writeError(w, 400, "invalid_request", "message ID must be a positive integer")
			return
		}
		switch r.Method {
		case http.MethodGet:
			value, err = backend.ListCommentActions(r.Context(), id)
		case http.MethodPost:
			var body CommentActionBody
			if !decodeEntityRequest(w, r, &body, "comment action") {
				return
			}
			value, err = backend.RunCommentAction(r.Context(), id, meta.ActionRequest{MessageID: id, Action: body.Action, Body: body.Body, DryRun: body.DryRun, Confirm: body.Confirm, ResolveUnknown: body.ResolveUnknown}, s.beginMutation("comment action"))
		case http.MethodPut:
			var body CommentTriageBody
			if !decodeEntityRequest(w, r, &body, "comment triage") {
				return
			}
			if !validCommentState(body.State) || (body.Expect != "" && !validCommentState(body.Expect)) {
				writeError(w, 400, "invalid_request", "Invalid comment triage state")
				return
			}
			var state string
			state, err = backend.UpdateCommentTriage(r.Context(), id, body.State, body.Expect, s.beginMutation("comment triage"))
			value = CommentTriageResponse{State: state}
		}
	}
	if err != nil {
		s.writeCommentActionError(w, r, err)
		return
	}
	writeJSON(w, 200, value)
}
func validCommentState(state string) bool {
	return state == "open" || state == "handled" || state == "skipped"
}
func (s *Server) writeCommentActionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errMutationGateBusy) {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	var failure *meta.ActionError
	if !errors.As(err, &failure) {
		writeError(w, 500, "local_failed", "Cannot access comment actions")
		return
	}
	status := 502
	switch failure.Code {
	case "invalid_request", "invalid_body", "not_a_comment", "target_deleted", "unsupported_source", "not_hideable", "ig_target_not_top_level", "target_hidden", "private_reply_not_allowed", "private_reply_window_closed", "confirmation_mismatch":
		status = 400
	case "action_not_enabled", "permission_missing", "app_access_insufficient", "token_invalid":
		status = 403
	case "message_not_found":
		status = 404
	case "stale_identity", "sync_active", "outcome_unknown", "already_sent", "triage_conflict":
		status = 409
	case "rate_limited":
		status = 429
	case "remote_accepted_local_failed", "local_failed":
		status = 500
	case "credential_unavailable":
		status = 503
	}
	writeJSON(w, status, failure)
}
