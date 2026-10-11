// Package twenty reads call recording evidence from a Twenty workspace.
package twenty

import (
	"context"
	"encoding/json/jsontext"
)

const (
	SourceType = "twenty"
	RawFormat  = "twenty_json"
)

type Source interface {
	Probe(ctx context.Context) error
	// ListRecordings returns recordings updated at or after updatedSince in
	// updatedAt order, each with its linked calendar event and participants.
	ListRecordings(ctx context.Context, updatedSince, cursor string, first int) (*Page, error)
}

type Recording struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ApplicationID names the installed Twenty app that wrote the recording,
	// such as Call Recorder or a Granola, Fathom or Fireflies integration.
	ApplicationID   string         `json:"applicationId"`
	CreatedAt       string         `json:"createdAt"`
	UpdatedAt       string         `json:"updatedAt"`
	StartedAt       string         `json:"startedAt"`
	CalendarEventID string         `json:"calendarEventId"`
	Raw             jsontext.Value `json:"-"`
	// Calendar is nil when the recording has no readable calendar event.
	Calendar *Calendar `json:"-"`
	// TooLarge marks a recording whose fields exceed the response bound; only
	// its ID and UpdatedAt are set.
	TooLarge bool `json:"-"`
}

type Page struct {
	Records    []Recording
	HasMore    bool
	NextCursor string
}

type Calendar struct {
	Raw          jsontext.Value
	Participants []jsontext.Value
}
