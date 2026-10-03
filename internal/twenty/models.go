// Package twenty reads Call Recorder evidence from a Twenty workspace.
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
	ListRecordings(ctx context.Context, cursor string, first int) (*Page, error)
	GetCalendar(ctx context.Context, id string) (*Calendar, error)
}

type Recording struct {
	ID              string         `json:"id"`
	Title           string         `json:"title"`
	Status          string         `json:"status"`
	CreatedAt       string         `json:"createdAt"`
	StartedAt       string         `json:"startedAt"`
	EndedAt         string         `json:"endedAt"`
	CalendarEventID string         `json:"calendarEventId"`
	Raw             jsontext.Value `json:"-"`
}

type Page struct {
	Records    []Recording
	HasMore    bool
	NextCursor string
}

type Participant struct {
	ID          string         `json:"id"`
	Handle      string         `json:"handle"`
	DisplayName string         `json:"displayName"`
	IsOrganizer bool           `json:"isOrganizer"`
	Raw         jsontext.Value `json:"-"`
}

type Calendar struct {
	Raw          jsontext.Value
	Participants []Participant
}
