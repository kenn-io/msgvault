// Package evidence defines Hex wire evidence without archive dependencies.
package evidence

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
)

const Version = 1

var ErrUnsupportedVersion = errors.New("unsupported Hex evidence version")

// Manifest is the allowlisted evidence retained from Hex schema 1. Track paths,
// export paths and local diagnostics deliberately stay outside the archive.
type Manifest struct {
	SchemaVersion    int       `json:"schema_version"`
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	TitleSource      string    `json:"title_source,omitempty"`
	ProvisionalTitle string    `json:"provisional_title,omitempty"`
	Status           string    `json:"status"`
	CreatedAtMS      int64     `json:"created_at_ms"`
	EndedAtMS        *int64    `json:"ended_at_ms,omitempty"`
	DurationMS       *int64    `json:"duration_ms,omitempty"`
	App              string    `json:"app,omitempty"`
	Notes            *Notes    `json:"notes,omitempty"`
	Calendar         *Calendar `json:"calendar,omitempty"`
	Chapters         []Chapter `json:"chapters,omitempty"`
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
	Manifest            Manifest `json:"manifest"`
	Entries             []Entry  `json:"entries,omitempty"`
	TranscriptState     string   `json:"transcript_state"`
	TranscriptPartial   bool     `json:"transcript_partial,omitempty"`
	TitleSourceInferred bool     `json:"title_source_inferred,omitempty"`
}

func (e Entry) Speaker() string {
	if e.Source == "microphone" {
		return "You"
	}
	return "Them"
}

// Envelope versions msgvault's evidence independently of Hex's source schema.
type Envelope struct {
	Version int     `json:"version"`
	Meeting Meeting `json:"meeting"`
}

func Decode(raw []byte) (Envelope, error) {
	var out Envelope
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if out.Version != Version {
		return out, fmt.Errorf("%w %d", ErrUnsupportedVersion, out.Version)
	}
	return out, out.Meeting.Validate()
}
func AttendeeEmail(value string) string {
	value = strings.TrimSpace(value)
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || !strings.Contains(value, "@") {
		return ""
	}
	return strings.ToLower(value)
}

// ResolveTitleSource runs on the recorder, whose local clock created legacy titles.
func (m *Manifest) ResolveTitleSource(location *time.Location) {
	if m.TitleSource != "" {
		return
	}
	title := "Meeting — " + time.UnixMilli(m.CreatedAtMS).In(location).Format("3:04 PM")
	if m.App != "" {
		title = m.App + " call — " + time.UnixMilli(m.CreatedAtMS).In(location).Format("3:04 PM")
	}
	m.TitleSource = "user"
	if m.Title == title {
		m.TitleSource = "default"
	}
}
func (m *Manifest) DisplayTitle() string {
	if m.TitleSource == "default" {
		if m.Notes != nil && strings.TrimSpace(m.Notes.Title) != "" {
			return strings.TrimSpace(m.Notes.Title)
		}
		if title := strings.TrimSpace(m.ProvisionalTitle); title != "" {
			return title
		}
	}
	return m.Title
}
func (m Meeting) HasTranscript() bool {
	for _, entry := range m.Entries {
		if strings.TrimSpace(entry.Text) != "" {
			return true
		}
	}
	return false
}
func ValidID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00") && strings.IndexFunc(id, unicode.IsControl) < 0
}
func ValidEntry(e Entry) bool {
	return (e.Source == "system" || e.Source == "microphone") && e.StartMS >= 0 && e.EndMS >= e.StartMS && e.EndMS <= 9223372036854
}
func (m Meeting) Validate() error {
	v := m.Manifest
	invalid := errors.New("invalid Hex meeting evidence")
	if !ValidID(v.ID) || v.SchemaVersion != 1 || v.CreatedAtMS <= 0 || v.CreatedAtMS > 253402300799999 {
		return invalid
	}
	if v.EndedAtMS != nil && (*v.EndedAtMS < v.CreatedAtMS || *v.EndedAtMS > 253402300799999) {
		return invalid
	}
	if v.DurationMS != nil && (*v.DurationMS < 0 || *v.DurationMS > 9223372036854) {
		return invalid
	}
	switch v.Status {
	case "complete", "interrupted", "failed":
	default:
		return invalid
	}
	switch v.TitleSource {
	case "", "default", "user", "calendar", "call":
	default:
		return invalid
	}
	switch m.TranscriptState {
	case "final", "rolling", "missing":
	default:
		return invalid
	}
	for _, e := range m.Entries {
		if !ValidEntry(e) || e.EndMS > 253402300799999-v.CreatedAtMS {
			return invalid
		}
	}
	for _, c := range v.Chapters {
		if c.StartMS < 0 || c.StartMS > 9223372036854 {
			return invalid
		}
	}
	return nil
}
