package plaud

import (
	"context"
	"time"
)

// Source is the read-only official Plaud MCP surface used by the importer.
type Source interface {
	CurrentUser(ctx context.Context) (string, error)
	ListFiles(ctx context.Context, page, pageSize int) (FilePage, error)
	Recording(ctx context.Context, id string) (Recording, error)
}

// File holds recording metadata; DurationMS is measured in milliseconds.
type File struct {
	ID         string
	Name       string
	StartedAt  time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DurationMS float64
	Blocks     []string
}

// FilePage includes the pagination checks supplied by Plaud, when present.
type FilePage struct {
	Files    []File
	Total    *int
	HasMore  *bool
	Complete *bool
}

// Segment is a speaker's transcript text with offsets from the recording start.
type Segment struct {
	Speaker      string  `json:"speaker"`
	Text         string  `json:"text"`
	StartSeconds float64 `json:"offset_seconds"`
	EndSeconds   float64 `json:"end_seconds,omitempty"`
}

// Note is one Plaud note tab, identified by its provider ID and data type.
type Note struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"data_type"`
	Content string `json:"content"`
}

// Recording combines file metadata, all note tabs, and the preferred transcript.
type Recording struct {
	File            File
	Notes           []Note
	Segments        []Segment
	TranscriptBlock string
}
