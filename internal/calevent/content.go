package calevent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

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
		return Content{Location: components[0].Location, Description: components[0].Description, JoinURL: components[0].JoinURL}, nil
	case CalDAVRawFormat:
		if len(metadata) == 0 {
			return Content{}, errors.New("CalDAV component selector missing or ambiguous")
		}
		var selector CalDAVMetadata
		if err := json.Unmarshal(metadata, &selector); err != nil {
			return Content{}, fmt.Errorf("CalDAV metadata: %w", err)
		}
		if strings.TrimSpace(selector.EventID) == "" {
			return Content{}, errors.New("CalDAV component selector missing or ambiguous")
		}
		zone := selector.TimeZone
		if zone == "" {
			zone = "UTC"
		}
		components, err := ParseCalDAVResource(ctx, raw, zone)
		if err != nil {
			return Content{}, err
		}
		for _, component := range components {
			if component.Event.ProviderEventID == selector.EventID {
				return Content{Location: component.Location, Description: component.Description, JoinURL: component.JoinURL}, nil
			}
		}
		return Content{}, errors.New("CalDAV component selector missing or ambiguous")
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
