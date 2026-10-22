package mcp

import (
	"context"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/store"
)

const (
	ToolListCalendarEvents = "list_calendar_events"
	ToolGetCalendarEvent   = "get_calendar_event"
)

type CalendarEventsBackend interface {
	ListCalendarOccurrencesContext(ctx context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error)
	GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error)
}

func listCalendarEventsDefinition() toolDefinition {
	properties := map[string]*jsonschema.Schema{
		"from":   meetingTimestampSchema("Inclusive RFC3339 window start"),
		"to":     meetingTimestampSchema("Exclusive RFC3339 window end, at most 400 days after from"),
		"limit":  boundedIntegerWithDefault("Maximum occurrences", 1, 500, 100),
		"cursor": stringSchema("next_cursor from the same window"),
	}
	description := untrustedCalendarContent +
		" List archived calendar occurrences. The response includes coverage.complete, coverage.notice, and coverage.sources." +
		" Archived results do not establish free time; use live free/busy for scheduling." +
		" Requires calendar.event.read or message.read on their sources. Narrow the window if query_too_broad is returned."
	d := readDefinition(ToolListCalendarEvents, description, closedObject(properties, "from", "to"), outputSchemaFor[store.CalendarOccurrencePage](), (*handlers).listCalendarEvents)
	d.availability = func(c catalogCapabilities) bool { return c.calEvents }
	return d
}

func getCalendarEventDefinition() toolDefinition {
	d := readDefinition(ToolGetCalendarEvent, untrustedCalendarContent+" Read an archived calendar occurrence. Requires calendar.event.read or message.read on its source.", closedObject(map[string]*jsonschema.Schema{"ref": stringSchema("Occurrence ref from list_calendar_events")}, "ref"), outputSchemaFor[store.CalendarOccurrenceDetail](), (*handlers).getCalendarEvent)
	d.availability = func(c catalogCapabilities) bool { return c.calEvents }
	return d
}

func (h *handlers) listCalendarEvents(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	from, err := time.Parse(time.RFC3339, stringArgument(args, "from"))
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	to, err := time.Parse(time.RFC3339, stringArgument(args, "to"))
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	query := store.CalendarOccurrenceQuery{From: from, To: to}
	query.Cursor, _ = args["cursor"].(string)
	if limit, ok := args["limit"].(float64); ok {
		query.Limit = new(int(limit))
	}
	page, err := h.calEvents.ListCalendarOccurrencesContext(ctx, query)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(page)
}

func (h *handlers) getCalendarEvent(ctx context.Context, req toolRequest) (*toolResult, error) {
	ref, _ := req.GetArguments()["ref"].(string)
	occurrence, err := h.calEvents.GetCalendarOccurrenceContext(ctx, ref)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(occurrence)
}
