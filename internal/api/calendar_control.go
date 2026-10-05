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
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/gcal"
)

// Calendar domain types share short names such as Person and Result with the
// archive. Prefix only the new calendar types to preserve existing wire names.
func calendarSchemaName(t reflect.Type, hint string) string {
	original := t
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	name := huma.DefaultSchemaNamer(original, hint)
	switch t.PkgPath() {
	case "go.kenn.io/msgvault/internal/gcal":
		return "GCal" + name
	case "go.kenn.io/msgvault/internal/calcontrol":
		return "Calendar" + name
	default:
		return name
	}
}

// CalendarController is implemented by the daemon adapter. It must apply both
// configured source policy and the authenticated delegated grant before writes.
type CalendarController interface {
	ControlCalendar(ctx context.Context, request calcontrol.Request, grant *agentgrant.Grant, acquireWrite func(context.Context) (func(), error)) (*calcontrol.Result, error)
}

var errCalendarGateBusy = errors.New("archive is busy or shutting down")

func (s *Server) registerCalendarControlRoute(api huma.API) {
	op := rawAPIV1Operation("controlCalendar", http.MethodPost, "/calendar/control", "Control a live calendar event or query availability")
	op.Tags = []string{"Calendar"}
	op.RequestBody = jsonRequestBodyFor[calcontrol.Request](api)
	op.Responses = jsonResponsesFor[calcontrol.Result](api)
	op.Errors = []int{400, 401, 403, 404, 409, 413, 415, 500, 502, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleCalendarControl)
}
func (s *Server) handleCalendarControl(w http.ResponseWriter, r *http.Request) {
	var request calcontrol.Request
	if !decodeCalendarControlRequest(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_calendar_request", err.Error())
		return
	}
	controller, ok := s.store.(CalendarController)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "calendar_unavailable", "Calendar control is unavailable")
		return
	}
	result, err := controller.ControlCalendar(r.Context(), request, s.requestAuthentication(r).Grant, s.beginCalendarMutation)
	if err != nil {
		if errors.Is(err, errCalendarGateBusy) {
			writeOperationGateBusy(w, r, s.operationGate)
			return
		}
		// A provider write can complete before its deadline or connection
		// fails. Preserve this outcome ahead of generic query retry advice.
		if errors.Is(err, gcal.ErrOutcomeUnknown) {
			writeError(w, http.StatusBadGateway, "calendar_outcome_unknown", "Provider outcome is unknown; reconcile the calendar state before taking further action")
			return
		}
		if s.writeIfContextError(w, err) {
			return
		}
		var missing *gcal.NotFoundError
		var conflict *gcal.PreconditionFailedError
		switch {
		case errors.Is(err, calcontrol.ErrDenied):
			writeError(w, http.StatusForbidden, "calendar_denied", err.Error())
		case errors.Is(err, calcontrol.ErrInvalid):
			writeError(w, http.StatusBadRequest, "invalid_calendar_request", err.Error())
		case errors.Is(err, calcontrol.ErrInternal):
			writeError(w, http.StatusInternalServerError, "calendar_internal", err.Error())
		case errors.Is(err, calcontrol.ErrPlanChanged):
			writeError(w, http.StatusConflict, "calendar_plan_changed", err.Error())
		case errors.As(err, &missing):
			writeError(w, http.StatusNotFound, "calendar_event_not_found", "Calendar event was not found")
		case errors.As(err, &conflict):
			writeError(w, http.StatusConflict, "calendar_event_conflict", err.Error())
		default:
			writeError(w, http.StatusBadGateway, "calendar_failed", err.Error())
		}
		return
	}
	if result == nil {
		writeError(w, http.StatusBadGateway, "calendar_failed", "Calendar operation returned no result")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) beginCalendarMutation(ctx context.Context) (func(), error) {
	if s.operationGate == nil {
		return func() {}, nil
	}
	done, ok := beginGateWorkBounded(ctx, s.operationGate, "calendar event change")
	if !ok {
		return nil, errCalendarGateBusy
	}
	return done, nil
}

func decodeCalendarControlRequest(w http.ResponseWriter, r *http.Request, destination *calcontrol.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid calendar request JSON")
		return false
	}
	if len(data) > 1<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Calendar request exceeds 1 MiB")
		return false
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return false
	}
	// These shared recursive validators also reject nulls and case aliases in
	// nested event fields, which could otherwise conceal a patch or guest change.
	if meetingJSONContainsNull(fields) || meetingJSONHasNoncanonicalField(fields, reflect.TypeOf(destination)) {
		writeError(w, http.StatusBadRequest, "bad_request", "Calendar fields must be non-null and use canonical casing")
		return false
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(data))
	if err := json.UnmarshalDecode(decoder, destination, json.RejectUnknownMembers(true)); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid calendar request JSON")
		return false
	}
	if _, err := decoder.ReadValue(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return false
	}
	return true
}
