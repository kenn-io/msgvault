// Package hex reads Hex's meeting folders without modifying the source.
package hex

import (
	"encoding/json/v2"
	"strings"
)

const SourceType = "hex"
const RawFormat = "hex_json"

// Manifest is the allowlisted evidence retained from Hex schema 1. Track paths,
// export paths and local diagnostics deliberately stay outside the archive.
type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Status        string    `json:"status"`
	CreatedAtMS   int64     `json:"created_at_ms"`
	EndedAtMS     *int64    `json:"ended_at_ms,omitempty"`
	DurationMS    *int64    `json:"duration_ms,omitempty"`
	App           string    `json:"app,omitempty"`
	Notes         *Notes    `json:"notes,omitempty"`
	Calendar      *Calendar `json:"calendar,omitempty"`
	Chapters      []Chapter `json:"chapters,omitempty"`
}
type Notes struct {
	Title   string  `json:"title"`
	Summary Summary `json:"summary"`
}

// Summary accepts both current text and Hex's legacy list of points.
type Summary string

func (s *Summary) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*s = Summary(text)
		return nil
	}
	var points []string
	if err := json.Unmarshal(data, &points); err != nil {
		return err
	}
	compact := points[:0]
	for _, point := range points {
		if point = strings.TrimSpace(point); point != "" {
			compact = append(compact, point)
		}
	}
	*s = Summary(strings.Join(compact, " "))
	return nil
}

type Calendar struct {
	Title     string   `json:"title"`
	Attendees []string `json:"attendees,omitempty"`
}
type Chapter struct {
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	StartMS int64  `json:"start_ms"`
}
type Entry struct {
	Source  string `json:"source"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}
type Meeting struct {
	Manifest        Manifest `json:"manifest"`
	Entries         []Entry  `json:"entries,omitempty"`
	TranscriptState string   `json:"transcript_state"`
}

func (e Entry) Speaker() string {
	if e.Source == "microphone" {
		return "You"
	}
	return "Them"
}
