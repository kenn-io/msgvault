package calevent

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

// Content is decoded only when opening an archived event. It is excluded from
// the event projection and its content hash.
type Content struct {
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
	JoinURL     string `json:"join_url,omitempty"`
}

func DecodeContent(ctx context.Context, format string, raw, metadata []byte, calendarZone string) (Content, error) {
	switch format {
	case "ics":
		components, _, err := ParseICSWithDiagnostics(ctx, raw, "", calendarZone, NewICSParseBudget())
		if err != nil {
			return Content{}, err
		}
		if len(components) != 1 {
			return Content{}, errors.New("stored ICS requires one VEVENT")
		}
		component := components[0]
		return Content{Location: component.Location, Description: component.Description, JoinURL: component.URL}, nil
	case CalDAVRawFormat:
		var selector CalDAVMetadata
		if err := json.Unmarshal(metadata, &selector); err != nil {
			return Content{}, err
		}
		events, err := ParseCalDAVResource(ctx, raw, calendarZone)
		if err != nil {
			return Content{}, err
		}
		for _, event := range events {
			if event.Event.ProviderEventID == selector.EventID {
				return Content{Location: event.Location, Description: event.Description, JoinURL: event.URL}, nil
			}
		}
		return Content{}, errors.New("CalDAV component selector is unavailable")
	case gcalmodel.RawFormat:
	default:
		return Content{}, nil
	}
	var event struct {
		Location       string `json:"location"`
		Description    string `json:"description"`
		HangoutLink    string `json:"hangoutLink"`
		ConferenceData struct {
			EntryPoints []struct {
				EntryPointType string `json:"entryPointType"`
				URI            string `json:"uri"`
			} `json:"entryPoints"`
		} `json:"conferenceData"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return Content{}, err
	}
	joinURL := event.HangoutLink
	if joinURL == "" {
		for _, entry := range event.ConferenceData.EntryPoints {
			if entry.EntryPointType == "video" {
				joinURL = entry.URI
				break
			}
		}
	}
	return Content{Location: event.Location, Description: event.Description, JoinURL: joinURL}, nil
}
