package mcp

import (
	"slices"

	"go.kenn.io/msgvault/internal/search"
)

func (h *handlers) calendarSearchHint(q *search.Query) string {
	if !slices.Contains(q.MessageTypes, "calendar_event") {
		return ""
	}
	const warning = "Archive results cannot prove free time."
	if h.calendarEventsAdvertised {
		return "Recurring meetings appear once as a series; use list_calendar_events for occurrences in a window. " + warning
	}
	return warning
}
