package gcalmodel

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
)

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
	Email            string `json:"email"`
	DisplayName      string `json:"displayName"`
	ResponseStatus   string `json:"responseStatus"`
	Organizer        bool   `json:"organizer"`
	Self             bool   `json:"self"`
	Resource         bool   `json:"resource"`
	Optional         bool   `json:"optional"`
	AdditionalGuests int    `json:"additionalGuests"`
	Comment          string `json:"comment"`
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
	return EventDateTime{DateTime: parseEventDateTime(w.DateTime, w.TimeZone), Date: w.Date, TimeZone: w.TimeZone}
}

func parseEventDateTime(value, zone string) time.Time {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	if zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			if t, err := time.ParseInLocation("2006-01-02T15:04:05", value, loc); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

type wireEvent struct {
	ETag              string             `json:"etag,omitempty"`
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
	Reminders         *Reminders         `json:"reminders"`
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
		ETag:              w.ETag,
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
		Reminders:         w.Reminders,
	}
	for _, a := range w.Attendees {
		ev.Attendees = append(ev.Attendees, Attendee(a))
	}
	return ev
}

// DecodeEvent unmarshals one event's JSON into the domain Event, preserving the
// original bytes in Event.Raw.
func DecodeEvent(raw []byte) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal(raw, &w); err != nil {
		return Event{}, err
	}
	ev := w.toEvent()
	ev.Raw = append(jsontext.Value(nil), raw...)
	return ev, nil
}
