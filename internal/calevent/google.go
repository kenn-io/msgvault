package calevent

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/calrecur"
)

type googleDateTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

func googleInstant(value googleDateTime, calendarZone string) (*time.Time, error) {
	if value.Date == "" && value.DateTime == "" {
		return nil, nil //nolint:nilnil // Sparse cancellation tombstones have no bound.
	}
	var instant time.Time
	var err error
	if value.DateTime != "" {
		instant, err = time.Parse(time.RFC3339, value.DateTime)
		if err == nil {
			utc := instant.UTC()
			return &utc, nil
		}
	}
	zone := value.TimeZone
	if zone == "" {
		zone = calendarZone
	}
	loc := time.UTC
	if zone != "" {
		loc, err = time.LoadLocation(zone)
		if err != nil {
			return nil, fmt.Errorf("calendar time zone: %w", err)
		}
	}
	if value.Date != "" {
		instant, _, err = calrecur.ResolveCivilDateBoundary(value.Date, loc)
	} else {
		instant, err = time.ParseInLocation("2006-01-02T15:04:05", value.DateTime, loc)
	}
	if err != nil {
		return nil, fmt.Errorf("calendar date/time: %w", err)
	}
	utc := instant.UTC()
	return &utc, nil
}

type googlePerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

type googleAttendee struct {
	googlePerson

	ResponseStatus string `json:"responseStatus"`
	Organizer      bool   `json:"organizer"`
	Optional       bool   `json:"optional"`
	Resource       bool   `json:"resource"`
	Self           bool   `json:"self"`
}

type googleEvent struct {
	ID                string           `json:"id"`
	RecurringEventID  string           `json:"recurringEventId"`
	OriginalStartTime googleDateTime   `json:"originalStartTime"`
	ICalUID           string           `json:"iCalUID"`
	Sequence          int64            `json:"sequence"`
	Status            string           `json:"status"`
	Start             googleDateTime   `json:"start"`
	End               googleDateTime   `json:"end"`
	Recurrence        []string         `json:"recurrence"`
	HangoutLink       string           `json:"hangoutLink"`
	Organizer         googlePerson     `json:"organizer"`
	Attendees         []googleAttendee `json:"attendees"`
	Transparency      string           `json:"transparency"`
	Visibility        string           `json:"visibility"`
	EventType         string           `json:"eventType"`
	Location          string           `json:"location"`
	Description       string           `json:"description"`
	ConferenceData    struct {
		EntryPoints []struct {
			EntryPointType string `json:"entryPointType"`
			URI            string `json:"uri"`
		} `json:"entryPoints"`
	} `json:"conferenceData"`
}

func readGoogle(raw []byte) (googleEvent, error) {
	var event googleEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return event, fmt.Errorf("decode Google calendar event: %w", err)
	}
	if event.ID == "" {
		return event, errors.New("calendar event has no id")
	}
	return event, nil
}

func init() { Register("gcal_json", decodeGoogleEvent) }

func decodeGoogleEvent(raw, metadata []byte, sc SourceContext) (Event, bool, error) {
	g, err := readGoogle(raw)
	if err != nil {
		return Event{}, false, err
	}
	var meta struct {
		Status     string `json:"status"`
		CalendarID string `json:"calendar_id"`
	}
	// Corrupt legacy metadata does not prevent decoding preserved raw evidence.
	if err := json.Unmarshal(metadata, &meta); err != nil {
		meta.Status, meta.CalendarID = "", ""
	}
	calendarID := meta.CalendarID
	if calendarID == "" {
		calendarID = sc.CalendarID
	}
	status := g.Status
	if meta.Status != "" {
		status = meta.Status
	}
	e := Event{
		Provider: "google", Namespace: calendarID, CalendarID: calendarID,
		ProviderEventID: g.ID, ICalUID: g.ICalUID, Sequence: g.Sequence, Status: status,
		Kind: "single", AllDay: g.Start.Date != "", StartDate: g.Start.Date, EndDate: g.End.Date,
		EventTimeZone: g.Start.TimeZone, CalendarTimeZone: sc.CalendarTimeZone,
		Recurrence: g.Recurrence, JoinURL: g.HangoutLink, OrganizerEmail: normalizeEmail(g.Organizer.Email),
		Transparency: g.Transparency, Visibility: g.Visibility, EventType: g.EventType, Location: g.Location,
	}
	e.Description = g.Description
	if len(e.Description) > 4096 {
		end := 4096
		for end > 0 && !utf8.RuneStart(e.Description[end]) {
			end--
		}
		e.Description, e.DescriptionTruncated = e.Description[:end], true
	}
	e.Start, err = googleInstant(g.Start, sc.CalendarTimeZone)
	if err != nil {
		return Event{}, false, err
	}
	e.End, err = googleInstant(g.End, sc.CalendarTimeZone)
	if err != nil {
		return Event{}, false, err
	}
	e.HasBounds = e.Start != nil && e.End != nil
	if g.RecurringEventID != "" {
		e.Kind, e.SeriesProviderID = "exception", g.RecurringEventID
		original := g.OriginalStartTime
		if original.Date != "" {
			if _, err := time.Parse(time.DateOnly, original.Date); err != nil {
				return Event{}, false, fmt.Errorf("calendar original date: %w", err)
			}
			e.Original = &calrecur.Identity{Date: original.Date, AllDay: true}
			if e.Start == nil {
				e.AllDay = true
			}
		} else if original.DateTime != "" {
			instant, err := googleInstant(original, sc.CalendarTimeZone)
			if err != nil {
				return Event{}, false, err
			}
			e.Original = &calrecur.Identity{Instant: *instant}
		} else if base, id, ok := calrecur.ParseInstanceID(g.ID); ok && base == g.RecurringEventID {
			e.Original = &id
			if e.Start == nil {
				e.AllDay = id.AllDay
			}
		}
	} else if len(g.Recurrence) != 0 {
		e.Kind, e.SeriesProviderID = "series", g.ID
	}
	if e.JoinURL == "" {
		for _, entry := range g.ConferenceData.EntryPoints {
			if entry.EntryPointType == "video" && entry.URI != "" {
				e.JoinURL = entry.URI
				break
			}
		}
	}
	e.JoinKey = JoinKey(e.JoinURL)
	for _, a := range g.Attendees {
		role := "required"
		switch {
		case IsRoomAttendee(a.Email, a.Resource):
			role = "room"
		case a.Organizer:
			role = "organizer"
		case a.Optional:
			role = "optional"
		}
		rsvp := "unknown"
		switch a.ResponseStatus {
		case "accepted", "declined", "tentative":
			rsvp = a.ResponseStatus
		case "needsAction":
			rsvp = "needs_action"
		}
		e.Attendees = append(e.Attendees, Attendee{Email: normalizeEmail(a.Email), DisplayName: a.DisplayName, Role: role, RSVP: rsvp, IsCopySelf: a.Self})
		if a.Self {
			e.CopySelfRSVP = rsvp
		}
	}
	return e, true, nil
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
