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

type File struct {
	ID         string
	Name       string
	StartedAt  time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DurationMS float64
	Blocks     []string
}

type FilePage struct {
	Files    []File
	Total    *int
	HasMore  *bool
	Complete *bool
}

type Segment struct {
	Speaker      string  `json:"speaker"`
	Text         string  `json:"text"`
	StartSeconds float64 `json:"offset_seconds"`
	EndSeconds   float64 `json:"end_seconds,omitempty"`
}

type Note struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"data_type"`
	Content string `json:"content"`
}

type Recording struct {
	File            File
	Notes           []Note
	Segments        []Segment
	TranscriptBlock string
}
