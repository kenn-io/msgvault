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

// CalendarEventsBackend uses the caller's daemon credential for archive reads.
type CalendarEventsBackend interface {
	ListCalendarEvents(ctx context.Context, filter store.CalendarOccurrenceFilter) (store.CalendarOccurrencePage, error)
	GetCalendarEvent(ctx context.Context, ref string) (store.CalendarOccurrence, error)
}

func listCalendarEventsDefinition() toolDefinition {
	d := readDefinition(ToolListCalendarEvents, "List archived calendar occurrences in a window of at most 400 days. Requires message.read; effective exception attendees and times replace the series values. Narrow the window or sources if query_too_broad is returned.", closedObject(map[string]*jsonschema.Schema{
		"from":              meetingTimestampSchema("Inclusive RFC3339 window start"),
		"to":                meetingTimestampSchema("Exclusive RFC3339 window end"),
		"source_ids":        meetingNonEmptyIDArraySchema("Source IDs narrowing the granted archive scope"),
		"attendee":          stringSchema("Exact effective attendee email"),
		"include_cancelled": {Type: "boolean", Description: "Include cancelled occurrences"},
		"limit":             boundedIntegerWithDefault("Maximum occurrences", 1, 500, 100),
		"cursor":            stringSchema("Opaque next_cursor from the same query"),
	}, "from", "to"), outputSchemaFor[store.CalendarOccurrencePage](), (*handlers).listCalendarEvents)
	d.availability = func(c catalogCapabilities) bool { return c.archiveCalendarEvents }
	return d
}

func getCalendarEventDefinition() toolDefinition {
	d := readDefinition(ToolGetCalendarEvent, "Read an archived calendar occurrence by ref. Requires message.read on its source.", closedObject(map[string]*jsonschema.Schema{"ref": stringSchema("Occurrence ref from list_calendar_events")}, "ref"), outputSchemaFor[store.CalendarOccurrence](), (*handlers).getCalendarEvent)
	d.availability = func(c catalogCapabilities) bool { return c.archiveCalendarEvents }
	return d
}

func (h *handlers) listCalendarEvents(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	from, err := meetingTimestampArg(args, "from")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if from == nil {
		return toolErrorResult("from must be an RFC3339 timestamp"), nil
	}
	to, err := meetingTimestampArg(args, "to")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if to == nil {
		return toolErrorResult("to must be an RFC3339 timestamp"), nil
	}
	if !to.After(*from) || to.Sub(*from) > 400*24*time.Hour {
		return toolErrorResult("window must be positive and at most 400 days"), nil
	}
	ids, err := positiveInt64ArrayArg(args, "source_ids")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	f := store.CalendarOccurrenceFilter{From: *from, To: *to, SourceIDs: ids}
	f.AttendeeEmail, _ = args["attendee"].(string)
	f.IncludeCancelled, _ = args["include_cancelled"].(bool)
	f.Cursor, _ = args["cursor"].(string)
	if _, ok := args["limit"]; ok {
		limit, err := positiveInt64Arg(args, "limit")
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}
		if limit > 500 {
			return toolErrorResult("limit must be an integer between 1 and 500"), nil
		}
		f.Limit = int(limit)
	}
	page, err := h.archiveCalendarEvents.ListCalendarEvents(ctx, f)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(page)
}

func (h *handlers) getCalendarEvent(ctx context.Context, req toolRequest) (*toolResult, error) {
	ref, _ := req.GetArguments()["ref"].(string)
	kind, _, _, err := store.ParseMeetingRef(ref)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if kind != "evt" {
		return toolErrorResult("ref must identify a calendar occurrence"), nil
	}
	occurrence, err := h.archiveCalendarEvents.GetCalendarEvent(ctx, ref)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	return jsonResult(occurrence)
}
