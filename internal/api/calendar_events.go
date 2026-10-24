package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

type CalendarEventStore interface {
	ListCalendarOccurrencesContext(ctx context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error)
	GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error)
}

func (s *Server) registerCalendarEventsRoute(api huma.API) {
	op := rawAPIV1Operation("listCalendarEvents", http.MethodGet, "/calendar/events", "List archived calendar occurrences")
	op.Tags = []string{"Calendar"}
	op.Parameters = []*huma.Param{
		{Name: "from", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString, Format: "date-time"}},
		{Name: "to", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString, Format: "date-time"}},
		{Name: "limit", In: "query", Schema: &huma.Schema{Type: "integer", Default: 100, Minimum: new(float64(1)), Maximum: new(float64(500))}},
		{Name: "cursor", In: "query", Schema: &huma.Schema{Type: huma.TypeString}},
	}
	op.Responses = jsonResponsesFor[store.CalendarOccurrencePage](api)
	op.Errors = []int{400, 401, 403, 500, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleCalendarEvents)
	detail := rawAPIV1Operation("getCalendarEvent", http.MethodGet, "/calendar/events/{ref}", "Read an archived calendar occurrence")
	detail.Tags = []string{"Calendar"}
	detail.Parameters = []*huma.Param{{Name: "ref", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
	detail.Responses = jsonResponsesFor[store.CalendarOccurrenceDetail](api)
	detail.Errors = []int{400, 401, 403, 404, 422, 500, 503}
	addErrorResponses(api, detail.Responses, detail.Errors...)
	registerRawHumaRoute(api, detail, s.handleCalendarEvent)
}
func (s *Server) handleCalendarEvents(w http.ResponseWriter, r *http.Request) {
	from, err := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	if err != nil {
		writeError(w, 400, "invalid_argument", "from must be RFC3339")
		return
	}
	to, err := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err != nil || to.Before(from) || to.Equal(from) && to.Format(time.DateOnly) <= from.Format(time.DateOnly) {
		writeError(w, 400, "invalid_argument", "to must be RFC3339 and after from")
		return
	}
	backend, ok := s.store.(CalendarEventStore)
	if !ok {
		writeError(w, 503, "calendar_unavailable", "Archived calendar events are unavailable")
		return
	}
	query := store.CalendarOccurrenceQuery{From: from, To: to, Cursor: r.URL.Query().Get("cursor")}
	if r.URL.Query().Has("limit") {
		value, err := strconv.Atoi(r.URL.Query().Get("limit"))
		query.Limit = &value
		if err != nil {
			writeError(w, 400, "invalid_argument", "limit must be an integer")
			return
		}
	}
	page, err := backend.ListCalendarOccurrencesContext(s.calendarReadContext(r), query)
	if err != nil {
		s.writeCalendarReadError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (s *Server) calendarReadContext(r *http.Request) context.Context {
	if s.requestAuthentication(r).Grant != nil {
		return sourcescope.WithSourceIDs(r.Context(), agentReadSourceIDs(r))
	}
	return r.Context()
}

func (s *Server) handleCalendarEvent(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.store.(CalendarEventStore)
	if !ok {
		writeError(w, 503, "calendar_unavailable", "Archived calendar events are unavailable")
		return
	}
	occurrence, err := backend.GetCalendarOccurrenceContext(s.calendarReadContext(r), r.PathValue("ref"))
	if err != nil {
		s.writeCalendarReadError(w, err)
		return
	}
	writeJSON(w, 200, occurrence)
}

func (s *Server) writeCalendarReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrQueryTooBroad):
		writeError(w, 400, "query_too_broad", err.Error())
	case errors.Is(err, store.ErrCalendarOccurrenceInvalid):
		writeError(w, 400, "invalid_argument", err.Error())
	case errors.Is(err, store.ErrCalendarOccurrenceNotFound):
		writeError(w, 404, "not_found", err.Error())
	case errors.Is(err, calrecur.ErrUnsupportedRecurrence):
		writeError(w, 422, "unsupported_recurrence", err.Error())
	default:
		if !s.writeIfContextError(w, err) {
			writeError(w, 500, "calendar_failed", err.Error())
		}
	}
}
