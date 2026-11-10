package mcp

import (
	"context"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/store"
)

const ToolListCalendarEvents = "list_calendar_events"

type CalendarEventsBackend interface {
	ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (store.CalendarOccurrencePage, error)
}

func listCalendarEventsDefinition() toolDefinition {
	properties := map[string]*jsonschema.Schema{
		"from": meetingTimestampSchema("Inclusive RFC3339 window start"),
		"to":   meetingTimestampSchema("Exclusive RFC3339 window end"),
	}
	d := readDefinition(ToolListCalendarEvents, untrustedCalendarContent+" List archived calendar occurrences. Requires calendar.event.read on their sources.", closedObject(properties, "from", "to"), outputSchemaFor[store.CalendarOccurrencePage](), (*handlers).listCalendarEvents)
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
	page, err := h.calEvents.ListCalendarOccurrencesContext(ctx, from, to)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(page)
}
