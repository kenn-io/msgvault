package plaud

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

type evidence struct {
	Version         int                          `json:"schema_version"`
	FileID          string                       `json:"file_id"`
	Title           string                       `json:"title"`
	StartedAt       time.Time                    `json:"started_at,omitzero"`
	DurationSeconds float64                      `json:"duration_seconds"`
	Summary         string                       `json:"summary_markdown"`
	NotesText       string                       `json:"notes"`
	Notes           []Note                       `json:"note_tabs"`
	Segments        []Segment                    `json:"transcript_segments"`
	TranscriptBlock string                       `json:"transcript_block,omitempty"`
	Speakers        []meetingcontent.Participant `json:"source_participants"`
}

func normalized(rec Recording) evidence {
	e := evidence{
		Version:         1,
		FileID:          rec.File.ID,
		Title:           rec.File.Name,
		StartedAt:       rec.File.StartedAt,
		DurationSeconds: rec.File.DurationMS / 1000,
		Notes:           rec.Notes,
		Segments:        rec.Segments,
		TranscriptBlock: rec.TranscriptBlock,
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = rec.File.CreatedAt
	}
	return e
}

// preserve keeps previously obtained evidence while provider processing is
// pending. Tabs match by stable ID, or by type only when it is unambiguous.
func (e *evidence) preserve(old evidence) {
	if len(e.Segments) == 0 {
		e.Segments = old.Segments
		e.TranscriptBlock = old.TranscriptBlock
	}
	if e.Title == "" {
		e.Title = old.Title
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = old.StartedAt
	}
	if e.DurationSeconds == 0 {
		e.DurationSeconds = old.DurationSeconds
	}
	countOld, countNew := map[string]int{}, map[string]int{}
	for _, n := range old.Notes {
		countOld[n.Type]++
	}
	for _, n := range e.Notes {
		countNew[n.Type]++
	}
	for _, n := range old.Notes {
		if strings.TrimSpace(n.Content) == "" {
			continue
		}
		match := -1
		for j, next := range e.Notes {
			sameID := n.ID != "" && next.ID == n.ID
			sameType := n.Type == next.Type
			uniqueType := sameType && countOld[n.Type] == 1 && countNew[n.Type] == 1
			sameAnonymous := sameType && n.ID == "" && next.ID == "" && n.Content == next.Content
			if sameID || uniqueType || sameAnonymous {
				match = j
				break
			}
		}
		if match < 0 {
			e.Notes = append(e.Notes, n)
		} else {
			if e.Notes[match].ID == "" {
				e.Notes[match].ID = n.ID
			}
			if strings.TrimSpace(e.Notes[match].Content) == "" {
				e.Notes[match].Content = n.Content
			}
		}
	}
}

func (e *evidence) snapshot(sourceID int64, email string) (meetingarchive.Snapshot, error) {
	var summary, notes, body strings.Builder
	seen := map[string]bool{}
	e.Speakers = []meetingcontent.Participant{}
	for _, n := range e.Notes {
		if strings.TrimSpace(n.Content) == "" {
			continue
		}
		if strings.Contains(n.Type, "sum") {
			if summary.Len() > 0 {
				summary.WriteString("\n\n")
			}
			summary.WriteString(n.Content)
		}
		if notes.Len() > 0 {
			notes.WriteString("\n\n")
		}
		notes.WriteString(n.Content)
	}
	e.Summary = summary.String()
	e.NotesText = notes.String()
	if e.NotesText != "" {
		body.WriteString("Notes:\n")
		body.WriteString(e.NotesText)
		body.WriteString("\n\n")
	}
	if len(e.Segments) > 0 {
		body.WriteString("Transcript:\n")
	}
	for _, seg := range e.Segments {
		fmt.Fprintf(&body, "[%02d:%02d] %s: %s\n", int(seg.StartSeconds)/60, int(seg.StartSeconds)%60, seg.Speaker, seg.Text)
		if !seen[seg.Speaker] {
			e.Speakers = append(e.Speakers, meetingcontent.Participant{Name: seg.Speaker, Role: "speaker"})
			seen[seg.Speaker] = true
		}
	}
	raw, err := json.Marshal(e, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	metadata, err := json.Marshal(map[string]any{
		"plaud_file_id":       e.FileID,
		"duration_seconds":    e.DurationSeconds,
		"source_participants": e.Speakers,
	}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	snippet := []rune(strings.TrimSpace(body.String()))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	return meetingarchive.Snapshot{
		SourceID:             sourceID,
		AccountEmail:         email,
		SourceMessageID:      e.FileID,
		SourceConversationID: e.FileID,
		Title:                e.Title,
		StartedAt:            e.StartedAt,
		Body:                 body.String(),
		Snippet:              string(snippet),
		Metadata:             metadata,
		Raw:                  raw,
		RawFormat:            "plaud_json",
	}, nil
}
