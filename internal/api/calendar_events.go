package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/verification"
)

type CalendarEventStore interface {
	ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (store.CalendarOccurrencePage, error)
}

func (s *Server) registerCalendarEventsRoute(api huma.API) {
	op := rawAPIV1Operation("listCalendarEvents", http.MethodGet, "/calendar/events", "List archived calendar occurrences")
	op.Tags = []string{"Calendar"}
	op.Parameters = []*huma.Param{
		{Name: "from", In: "query", Required: true, Schema: &huma.Schema{Type: "string", Format: "date-time"}},
		{Name: "to", In: "query", Required: true, Schema: &huma.Schema{Type: "string", Format: "date-time"}},
	}
	op.Responses = jsonResponsesFor[store.CalendarOccurrencePage](api)
	op.Errors = []int{400, 401, 403, 500, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleCalendarEvents)
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
	ctx := r.Context()
	if s.requestAuthentication(r).Grant != nil {
		ids := agentReadSourceIDs(r)
		if sourcescope.Restricted(ctx) {
			allowed := sourcescope.IDs(ctx)
			ids = slices.DeleteFunc(slices.Clone(ids), func(id int64) bool { return !slices.Contains(allowed, id) })
		}
		ctx = sourcescope.WithSourceIDs(ctx, ids)
	}
	page, err := backend.ListCalendarOccurrencesContext(ctx, from, to)
	if err != nil {
		if errors.Is(err, store.ErrQueryTooBroad) {
			writeError(w, 400, "query_too_broad", err.Error())
			return
		}
		if s.writeIfContextError(w, err) {
			return
		}
		writeError(w, 500, "calendar_failed", err.Error())
		return
	}
	ids := make([]int64, 0, len(page.Occurrences)*2)
	for _, row := range page.Occurrences {
		ids = append(ids, row.MessageID)
		if row.SeriesMessageID != 0 {
			ids = append(ids, row.SeriesMessageID)
		}
	}
	withheld, ok := s.verificationWithheld(w, r, ids)
	if !ok {
		return
	}
	for i := range page.Occurrences {
		row := &page.Occurrences[i]
		if withheld[row.MessageID] || withheld[row.SeriesMessageID] {
			row.Subject = verification.Masked
		}
	}
	writeJSON(w, 200, page)
}
