package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/store"
)

// CalendarEventsBackend reads archived occurrences inside the admitted source scope.
type CalendarEventsBackend interface {
	ListCalendarOccurrencesContext(ctx context.Context, scope store.SourceScope, filter store.CalendarOccurrenceFilter) (store.CalendarOccurrencePage, error)
	GetCalendarOccurrenceContext(ctx context.Context, scope store.SourceScope, ref string) (store.CalendarOccurrence, error)
}

func (s *Server) registerCalendarEventsRoutes(api huma.API) {
	limitParam := queryIntegerParam("limit", "Maximum occurrences (default 100, maximum 500)")
	minimum, maximum := float64(1), float64(500)
	limitParam.Schema.Minimum = &minimum
	limitParam.Schema.Maximum = &maximum
	limitParam.Schema.Default = 100
	list := rawAPIV1Operation("listCalendarEvents", http.MethodGet, "/calendar/events", "List archived calendar occurrences")
	list.Tags = []string{"Calendar"}
	list.Parameters = []*huma.Param{
		queryStringParam("from", "Inclusive RFC3339 window start", true),
		queryStringParam("to", "Exclusive RFC3339 window end; window at most 400 days", true),
		queryStringParam("source_ids", "Comma-separated source IDs narrowing the admitted scope", false),
		queryStringParam("attendee", "Exact effective attendee email", false),
		queryBooleanParam("include_cancelled", "Include cancelled occurrences"),
		limitParam,
		queryStringParam("cursor", "Opaque next_cursor from the same query", false),
	}
	list.Responses = jsonResponsesFor[store.CalendarOccurrencePage](api)
	list.Errors = []int{400, 401, 403, 422, 503, 500}
	addErrorResponses(api, list.Responses, list.Errors...)
	registerRawHumaRoute(api, list, s.handleListCalendarEvents)
	get := rawAPIV1Operation("getCalendarEvent", http.MethodGet, "/calendar/events/{ref}", "Read one archived calendar occurrence")
	get.Tags = list.Tags
	get.Parameters = []*huma.Param{{Name: "ref", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}, Description: "Occurrence ref returned by listCalendarEvents"}}
	get.Responses = jsonResponsesFor[store.CalendarOccurrence](api)
	get.Errors = []int{400, 401, 403, 404, 422, 503, 500}
	addErrorResponses(api, get.Responses, get.Errors...)
	registerRawHumaRoute(api, get, s.handleGetCalendarEvent)
}

func (s *Server) calendarEventsScope(r *http.Request) store.SourceScope {
	if s.requestAuthentication(r).Grant == nil {
		return store.OwnerScope()
	}
	return store.SourceScope{IDs: agentReadSourceIDs(r)}
}

func (s *Server) handleListCalendarEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := time.Parse(time.RFC3339, q.Get("from"))
	if err != nil {
		writeError(w, 400, "invalid_argument", "from must be an RFC3339 timestamp")
		return
	}
	to, err := time.Parse(time.RFC3339, q.Get("to"))
	if err != nil {
		writeError(w, 400, "invalid_argument", "to must be an RFC3339 timestamp")
		return
	}
	ids, _, err := queryInt64s(r, "source_ids")
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	limit, hasLimit, err := queryInt(r, "limit")
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	if hasLimit && (limit < 1 || limit > 500) {
		writeError(w, 400, "invalid_argument", "limit must be between 1 and 500")
		return
	}
	cancelled, _, err := queryBool(r, "include_cancelled")
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	backend, ok := s.store.(CalendarEventsBackend)
	if !ok {
		writeError(w, 503, "calendar_events_unavailable", "Archived calendar events are unavailable")
		return
	}
	page, err := backend.ListCalendarOccurrencesContext(r.Context(), s.calendarEventsScope(r), store.CalendarOccurrenceFilter{
		From: from, To: to, SourceIDs: ids, Limit: limit, IncludeCancelled: cancelled, AttendeeEmail: q.Get("attendee"), Cursor: q.Get("cursor"),
	})
	if err != nil {
		s.writeCalendarEventsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetCalendarEvent(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.store.(CalendarEventsBackend)
	if !ok {
		writeError(w, 503, "calendar_events_unavailable", "Archived calendar events are unavailable")
		return
	}
	occurrence, err := backend.GetCalendarOccurrenceContext(r.Context(), s.calendarEventsScope(r), r.PathValue("ref"))
	if err != nil {
		s.writeCalendarEventsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, occurrence)
}

func (s *Server) writeCalendarEventsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrQueryTooBroad):
		writeError(w, 400, "query_too_broad", "Calendar query is too broad; narrow the window or sources")
	case errors.Is(err, store.ErrInvalidArgument):
		writeError(w, 400, "invalid_argument", err.Error())
	case errors.Is(err, calrecur.ErrUnsupportedRecurrence):
		writeError(w, http.StatusUnprocessableEntity, "recurrence_unsupported", "Calendar occurrence belongs to an unsupported recurrence series")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "not_found", "Calendar occurrence not found")
	default:
		writeError(w, 500, "internal_error", "Failed to read archived calendar events")
	}
}
