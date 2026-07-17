package gcal

import (
	"encoding/json"
	"time"
)

// Unexported JSON wire types for the Calendar API v3, mapped to the exported
// domain types. Each maps via a toX() method so the rest of the package never
// sees raw JSON shapes.

type wireCalendarListEntry struct {
	ID              string `json:"id"`
	Summary         string `json:"summary"`
	SummaryOverride string `json:"summaryOverride"`
	Description     string `json:"description"`
	TimeZone        string `json:"timeZone"`
	AccessRole      string `json:"accessRole"`
	Primary         bool   `json:"primary"`
	Deleted         bool   `json:"deleted"`
}

func (w wireCalendarListEntry) toCalendar() Calendar {
	summary := w.Summary
	if w.SummaryOverride != "" {
		// A user-set override (e.g. a renamed subscribed calendar) is the
		// label the user actually sees; prefer it.
		summary = w.SummaryOverride
	}
	return Calendar{
		ID:          w.ID,
		Summary:     summary,
		Description: w.Description,
		TimeZone:    w.TimeZone,
		AccessRole:  w.AccessRole,
		Primary:     w.Primary,
		Deleted:     w.Deleted,
	}
}

type wireCalendarList struct {
	Items         []wireCalendarListEntry `json:"items"`
	NextPageToken string                  `json:"nextPageToken"`
}

type wirePerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Self        bool   `json:"self"`
}

func (w *wirePerson) toPerson() Person {
	if w == nil {
		return Person{}
	}
	return Person{Email: w.Email, DisplayName: w.DisplayName, Self: w.Self}
}

type wireAttendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	ResponseStatus string `json:"responseStatus"`
	Organizer      bool   `json:"organizer"`
	Self           bool   `json:"self"`
	Resource       bool   `json:"resource"`
	Optional       bool   `json:"optional"`
}

type wireEventDateTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
	TimeZone string `json:"timeZone"`
}

func (w *wireEventDateTime) toEventDateTime() EventDateTime {
	if w == nil {
		return EventDateTime{}
	}
	var dt time.Time
	if w.DateTime != "" {
		if t, err := time.Parse(time.RFC3339, w.DateTime); err == nil {
			dt = t
		}
	}
	return EventDateTime{DateTime: dt, Date: w.Date, TimeZone: w.TimeZone}
}

type wireEvent struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	HTMLLink          string             `json:"htmlLink"`
	HangoutLink       string             `json:"hangoutLink"`
	Created           string             `json:"created"`
	Updated           string             `json:"updated"`
	Summary           string             `json:"summary"`
	Description       string             `json:"description"`
	Location          string             `json:"location"`
	Creator           *wirePerson        `json:"creator"`
	Organizer         *wirePerson        `json:"organizer"`
	Start             *wireEventDateTime `json:"start"`
	End               *wireEventDateTime `json:"end"`
	Recurrence        []string           `json:"recurrence"`
	RecurringEventID  string             `json:"recurringEventId"`
	OriginalStartTime *wireEventDateTime `json:"originalStartTime"`
	ICalUID           string             `json:"iCalUID"`
	Sequence          int                `json:"sequence"`
	Attendees         []wireAttendee     `json:"attendees"`
	Transparency      string             `json:"transparency"`
	Visibility        string             `json:"visibility"`
	EventType         string             `json:"eventType"`
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func (w wireEvent) toEvent() Event {
	ev := Event{
		ID:                w.ID,
		Status:            w.Status,
		HTMLLink:          w.HTMLLink,
		HangoutLink:       w.HangoutLink,
		Created:           parseRFC3339(w.Created),
		Updated:           parseRFC3339(w.Updated),
		Summary:           w.Summary,
		Description:       w.Description,
		Location:          w.Location,
		Creator:           w.Creator.toPerson(),
		Organizer:         w.Organizer.toPerson(),
		Start:             w.Start.toEventDateTime(),
		End:               w.End.toEventDateTime(),
		Recurrence:        w.Recurrence,
		RecurringEventID:  w.RecurringEventID,
		OriginalStartTime: w.OriginalStartTime.toEventDateTime(),
		ICalUID:           w.ICalUID,
		Sequence:          w.Sequence,
		Transparency:      w.Transparency,
		Visibility:        w.Visibility,
		EventType:         w.EventType,
	}
	for _, a := range w.Attendees {
		ev.Attendees = append(ev.Attendees, Attendee(a))
	}
	return ev
}

type wireEvents struct {
	// Items are kept as raw JSON so each event's original bytes can be
	// preserved verbatim in Event.Raw for the archive.
	Items         []json.RawMessage `json:"items"`
	NextPageToken string            `json:"nextPageToken"`
	NextSyncToken string            `json:"nextSyncToken"`
	TimeZone      string            `json:"timeZone"`
}

// decodeEvent unmarshals one event's JSON into the domain Event, preserving the
// original bytes in Event.Raw.
func decodeEvent(raw []byte) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal(raw, &w); err != nil {
		return Event{}, err
	}
	ev := w.toEvent()
	ev.Raw = append(json.RawMessage(nil), raw...)
	return ev, nil
}
