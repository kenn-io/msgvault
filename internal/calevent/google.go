package calevent

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

func googleInstant(value gcalmodel.EventDateTime, calendarZone string) (*time.Time, error) {
	if value.Date == "" && value.DateTime.IsZero() {
		return nil, nil //nolint:nilnil // Sparse cancellation tombstones have no bound.
	}
	if !value.DateTime.IsZero() {
		utc := value.DateTime.UTC()
		return &utc, nil
	}
	var instant time.Time
	var err error
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
		instant, _, err = calrecur.CivilDateBoundary(value.Date, loc)
	}
	if err != nil {
		return nil, fmt.Errorf("calendar date/time: %w", err)
	}
	utc := instant.UTC()
	return &utc, nil
}

func decodeGoogleEvent(raw, metadata []byte, calendarZone string) (Event, bool, error) {
	g, err := gcalmodel.DecodeEvent(raw)
	if err != nil {
		return Event{}, false, err
	}
	if g.ID == "" {
		return Event{}, false, errors.New("calendar event has no id")
	}
	var meta struct {
		Status string `json:"status"`
	}
	// Corrupt legacy metadata does not prevent decoding preserved raw evidence.
	if err := json.Unmarshal(metadata, &meta); err != nil {
		meta.Status = ""
	}
	status := g.Status
	if meta.Status != "" {
		status = meta.Status
	}
	if status == "" {
		status = "confirmed"
	}
	e := Event{
		ProviderEventID: g.ID, Status: status, Kind: "single", AllDay: g.Start.Date != "", StartDate: g.Start.Date, EndDate: g.End.Date,
		EventTimeZone: g.Start.TimeZone, CalendarTimeZone: calendarZone, Recurrence: g.Recurrence,
	}
	e.Start, err = googleInstant(g.Start, calendarZone)
	if err != nil {
		return Event{}, false, err
	}
	e.End, err = googleInstant(g.End, calendarZone)
	if err != nil {
		return Event{}, false, err
	}
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
		} else if !original.DateTime.IsZero() {
			instant, err := googleInstant(original, calendarZone)
			if err != nil {
				return Event{}, false, err
			}
			e.Original = &calrecur.Identity{Instant: *instant}
		}
	} else if len(g.Recurrence) != 0 {
		e.Kind, e.SeriesProviderID = "series", g.ID
	}
	organizerEmail := gcalmodel.NormalizeParticipantEmail(g.Organizer.Email)
	organizerSeen := false
	for _, a := range g.Attendees {
		email := gcalmodel.NormalizeParticipantEmail(a.Email)
		if organizerEmail != "" && email == organizerEmail {
			a.Organizer = true
			organizerSeen = true
		}
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
		e.Attendees = append(e.Attendees, Attendee{Email: email, DisplayName: a.DisplayName, Role: role, RSVP: rsvp, IsCopySelf: a.Self})
	}
	if organizerEmail != "" && !organizerSeen {
		e.Attendees = append(e.Attendees, Attendee{Email: organizerEmail, DisplayName: g.Organizer.DisplayName, Role: "organizer", RSVP: "unknown", IsCopySelf: g.Organizer.Self})
	}
	return e, true, nil
}
