package calevent

import (
	"encoding/json/v2"

	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

// Content is decoded only when opening an archived event. It is excluded from
// the event projection and its content hash.
type Content struct {
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
	JoinURL     string `json:"join_url,omitempty"`
}

func DecodeContent(format string, raw []byte) (Content, error) {
	if format != gcalmodel.RawFormat {
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
