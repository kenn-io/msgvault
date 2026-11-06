package calevent

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

// SerializeBody is the single body_text shared by FTS body and embeddings:
// title, time range, location, description, and attendee DISPLAY NAMES. Raw
// attendee email addresses are deliberately excluded (they reach FTS via the
// toAddrs column only).
func SerializeBody(ev gcalmodel.Event) string {
	var b strings.Builder
	writeLine := func(s string) {
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	writeLine(ev.Summary)
	writeLine(whenLine(ev))
	if ev.Location != "" {
		writeLine("Location: " + ev.Location)
	}
	writeLine(ev.Description)

	var names []string
	for _, a := range ev.Attendees {
		if a.DisplayName != "" {
			names = append(names, a.DisplayName)
		}
	}
	if len(names) > 0 {
		writeLine("Attendees: " + strings.Join(names, ", "))
	}
	return strings.TrimSpace(b.String())
}

// whenLine renders a human/searchable time range.
func whenLine(ev gcalmodel.Event) string {
	start, ok := ev.Start.Instant()
	if !ok {
		return ""
	}
	if ev.Start.IsAllDay() {
		return "When: " + start.Format("2006-01-02") + " (all day)"
	}
	if end, ok := ev.End.Instant(); ok {
		return "When: " + start.Format("2006-01-02 15:04") + " - " + end.Format("2006-01-02 15:04")
	}
	return "When: " + start.Format("2006-01-02 15:04")
}

// Snippet returns a trimmed calendar-event preview of at most 200 bytes without splitting valid UTF-8.
func Snippet(body string) string {
	const maxSnippetBytes = 200
	body = strings.TrimSpace(body)
	if len(body) <= maxSnippetBytes {
		return body
	}

	end := maxSnippetBytes
	for end > 0 && !utf8.RuneStart(body[end]) {
		end--
	}
	return body[:end]
}

// MessageEvent maps a decoded CalDAV component to the shared message renderer.
func (item CalDAVEvent) MessageEvent() (gcalmodel.Event, error) {
	e := item.Event
	var err error
	bodyEvent := gcalmodel.Event{Summary: item.Summary, Description: item.Description, Location: item.Location}
	if e.AllDay {
		bodyEvent.Start.Date, bodyEvent.End.Date = e.StartDate, e.EndDate
	} else {
		zoneName := e.EventTimeZone
		if zoneName == "" {
			zoneName = e.CalendarTimeZone
		}
		zone := time.UTC
		if zoneName != "" {
			zone, err = calrecur.LoadZone(zoneName)
			if err != nil {
				return gcalmodel.Event{}, fmt.Errorf("CalDAV body time zone: %w", err)
			}
		}
		if e.Start != nil {
			bodyEvent.Start.DateTime = e.Start.In(zone)
		}
		if e.End != nil {
			bodyEvent.End.DateTime = e.End.In(zone)
		}
	}
	for _, a := range e.Attendees {
		bodyEvent.Attendees = append(bodyEvent.Attendees, gcalmodel.Attendee{DisplayName: a.DisplayName})
	}
	if e.Original != nil {
		if e.Original.AllDay {
			bodyEvent.OriginalStartTime.Date = e.Original.Date
		} else {
			bodyEvent.OriginalStartTime.DateTime = e.Original.Instant
		}
	}
	if e.Kind == "exception" {
		bodyEvent.RecurringEventID = e.SeriesProviderID
	}
	return bodyEvent, nil
}
