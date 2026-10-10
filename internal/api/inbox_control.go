package api

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxController owns binding resolution, source leases and durable receipts.
// The supplied authorization callback rechecks the current authenticated grant
// before replay and dispatch, rather than retaining its admission-time copy.
type InboxController interface {
	ControlInbox(ctx context.Context, request inboxcontrol.Request, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) (*inboxcontrol.Result, error)
}

type InboxControlError struct {
	Error   string               `json:"error"`
	Message string               `json:"message"`
	Result  *inboxcontrol.Result `json:"result,omitempty"`
}

func (s *Server) registerInboxControlRoute(api huma.API) {
	op := rawAPIV1Operation("controlInbox", http.MethodPost, "/inbox/control", "Preview, execute or reconcile one exact inbox action")
	op.Tags = []string{"Inbox"}
	op.RequestBody = jsonRequestBodyFor[inboxcontrol.Request](api)
	op.Responses = jsonResponsesFor[inboxcontrol.Result](api)
	for key, response := range jsonResponsesFor[InboxControlError](api, 400, 401, 403, 409, 413, 415, 500, 501, 502, 503) {
		if key != defaultErrorResponse {
			op.Responses[key] = response
		}
	}
	registerRawHumaRoute(api, op, s.handleInboxControl)
}

func (s *Server) handleInboxControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request inboxcontrol.Request
	if !decodeInboxControlRequest(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_inbox_request", err.Error())
		return
	}
	principal, authorize, err := s.inboxAuthorization(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
		return
	}

	// Receipt authority comes from the stored intent inside the controller.
	if request.Target != nil || request.Source != nil {
		if err := authorize(r.Context(), principal, request); err != nil {
			writeError(w, http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error())
			return
		}
	}
	controller, ok := s.store.(InboxController)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error())
		return
	}
	result, err := controller.ControlInbox(r.Context(), request, principal, authorize, s.beginInboxMutation)
	if err != nil {
		if errors.Is(err, errCalendarGateBusy) {
			writeOperationGateBusy(w, r, s.operationGate)
			return
		}
		status, code, message := inboxControlErrorStatus(err)
		writeJSON(w, status, InboxControlError{Error: code, Message: message, Result: result})
		return
	}
	if result == nil {
		writeError(w, http.StatusBadGateway, "inbox_failed", "Inbox operation returned no result")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) beginInboxMutation(ctx context.Context) (func(), error) {
	if s.operationGate == nil {
		return func() {}, nil
	}
	done, ok := beginGateWorkBounded(ctx, s.operationGate, "inbox operation")
	if !ok {
		return nil, errCalendarGateBusy
	}
	return done, nil
}

func decodeInboxControlRequest(w http.ResponseWriter, r *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid inbox request JSON")
		return false
	}
	if len(data) > 1<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Inbox request exceeds 1 MiB")
		return false
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil || meetingJSONContainsNull(fields) || meetingJSONHasNoncanonicalField(fields, reflect.TypeOf(destination)) {
		writeError(w, http.StatusBadRequest, "bad_request", "Inbox fields must be non-null and use canonical casing")
		return false
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(data))
	if err := json.UnmarshalDecode(decoder, destination, json.RejectUnknownMembers(true)); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid inbox request JSON")
		return false
	}
	if _, err := decoder.ReadValue(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return false
	}
	return true
}

// inboxAuthorization keeps the original authenticated principal while checking
// the current request credential, source and action grants at every boundary.
func (s *Server) inboxAuthorization(r *http.Request) (inboxcontrol.Principal, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, error) {
	auth := s.requestAuthentication(r)
	principal := inboxcontrol.Principal{ID: "owner", Owner: true}
	if auth.Mode == AuthModeDelegated && auth.Grant != nil {
		principal = inboxcontrol.Principal{ID: auth.Grant.ID}
	} else if auth.Mode == AuthModeRequired {
		return inboxcontrol.Principal{}, nil, inboxcontrol.ErrDenied
	}
	authorize := func(_ context.Context, caller inboxcontrol.Principal, intent inboxcontrol.Request) error {
		// Read credentials from this request again; admission-time security
		// metadata intentionally remains immutable for middleware decisions.
		current := s.classifyAPIRequestDirect(r)
		if current.Mode == AuthModeRequired {
			return inboxcontrol.ErrDenied
		}
		if caller.Owner {
			if caller.ID != "owner" || current.Mode == AuthModeDelegated {
				return inboxcontrol.ErrDenied
			}
			return nil
		}
		if current.Mode != AuthModeDelegated || current.Grant == nil || current.Grant.ID != caller.ID {
			return inboxcontrol.ErrDenied
		}
		var source agentgrant.SourceRef
		if intent.Target != nil {
			source = agentgrant.SourceRef{ID: intent.Target.SourceID, Type: intent.Target.SourceType, Identifier: intent.Target.SourceIdentifier}
		} else if intent.Source != nil {
			source = agentgrant.SourceRef{ID: intent.Source.SourceID, Type: intent.Source.SourceType, Identifier: intent.Source.SourceIdentifier}
		} else {
			return inboxcontrol.ErrDenied
		}
		permission, err := intent.Operation.RequiredPermission()
		if err != nil || !current.Grant.Allows(agentgrant.PermissionInboxRead, source) || !current.Grant.Allows(permission, source) {
			return inboxcontrol.ErrDenied
		}
		return nil
	}
	return principal, authorize, nil
}

func inboxControlErrorStatus(err error) (int, string, string) {
	status, code, message := http.StatusBadGateway, "inbox_failed", "Unable to complete inbox operation"
	switch {
	case errors.Is(err, inboxcontrol.ErrDenied):
		status, code, message = http.StatusForbidden, "inbox_denied", inboxcontrol.ErrDenied.Error()
	case errors.Is(err, inboxcontrol.ErrInvalid):
		status, code, message = http.StatusBadRequest, "invalid_inbox_request", inboxcontrol.ErrInvalid.Error()
	case errors.Is(err, inboxcontrol.ErrPlanChanged), errors.Is(err, inboxcontrol.ErrConflict):
		status, code, message = http.StatusConflict, "inbox_conflict", "Inbox preview or operation conflicts with current evidence"
	case errors.Is(err, inboxcontrol.ErrUnavailable):
		status, code, message = http.StatusServiceUnavailable, "inbox_unavailable", inboxcontrol.ErrUnavailable.Error()
	case errors.Is(err, inboxcontrol.ErrInternal):
		status, code, message = http.StatusInternalServerError, "inbox_internal", inboxcontrol.ErrInternal.Error()
	case errors.Is(err, inboxcontrol.ErrOutcomeUnknown):
		code, message = "inbox_outcome_unknown", inboxcontrol.ErrOutcomeUnknown.Error()
	case errors.Is(err, inboxcontrol.ErrReconcileOnly):
		status, code, message = http.StatusInternalServerError, "inbox_reconcile_only", inboxcontrol.ErrReconcileOnly.Error()
	case errors.Is(err, inboxcontrol.ErrNoWrite):
		status, code, message = http.StatusConflict, "inbox_provider_rejected", inboxcontrol.ErrNoWrite.Error()
	}
	return status, code, message
}
