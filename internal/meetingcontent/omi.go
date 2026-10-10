package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

func decodeOmi(fields map[string]jsontext.Value) Content {
	content := baseRecognizedContent()
	content.Notes = Section{State: StateUnsupported}
	var structured map[string]jsontext.Value
	if json.Unmarshal(fields["structured"], &structured) != nil || structured == nil {
		content.Summary = Section{State: StateUnavailable, Reason: reasonInvalidSection}
		content.ActionCoverage, content.ActionReason = CoverageUnavailable, reasonInvalidSection
	} else {
		content.Summary = decodeStringField(structured, "overview")
		content.Actions, content.ActionCoverage, content.ActionReason = decodeOmiActions(structured)
	}
	var span float64
	content.Transcript, span = decodeOmiTranscript(fields)
	start, startOK := rawTime(fields["started_at"], false)
	end, endOK := rawTime(fields["finished_at"], false)
	if startOK && endOK && end.After(start) {
		setDuration(&content, end.Sub(start).Seconds(), DurationProvider)
	} else if span > 0 {
		setDuration(&content, span, DurationTranscriptSpan)
	}
	// Speaker names are display evidence, not verified email identities.
	seen := map[string]bool{}
	for _, s := range content.Transcript.Segments {
		if !seen[s.Speaker] {
			content.SourceParticipants = append(content.SourceParticipants, Participant{Name: s.Speaker, Role: "speaker"})
			seen[s.Speaker] = true
		}
	}
	return content
}

func decodeOmiTranscript(fields map[string]jsontext.Value) (Transcript, float64) {
	var first, last float64
	found := false
	raw, exists := fields["transcript_segments"]
	if !exists {
		return Transcript{State: StateUnavailable, Reason: reasonMissingField}, 0
	}
	if isNull(raw) {
		return Transcript{State: StateUnavailable, Reason: reasonMissingField}, 0
	}
	var items []jsontext.Value
	if json.Unmarshal(raw, &items) != nil {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}, 0
	}
	segments := make([]Segment, 0, len(items))
	for _, item := range items {
		var wire struct {
			Text      *string  `json:"text"`
			Name      string   `json:"speaker_name"`
			SpeakerID *int     `json:"speaker_id"`
			Start     *float64 `json:"start"`
			End       *float64 `json:"end"`
		}
		if json.Unmarshal(item, &wire) != nil || wire.Text == nil {
			return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}, 0
		}
		if wire.Start != nil && wire.End != nil && *wire.Start >= 0 && *wire.End >= *wire.Start {
			if !found {
				first, last, found = *wire.Start, *wire.End, true
			} else {
				first, last = min(first, *wire.Start), max(last, *wire.End)
			}
		}
		if strings.TrimSpace(*wire.Text) == "" {
			continue
		}
		if (wire.Start != nil && *wire.Start < 0) || (wire.End != nil && *wire.End < 0) || (wire.Start != nil && wire.End != nil && *wire.End < *wire.Start) {
			wire.Start = nil
		}
		speaker := strings.TrimSpace(wire.Name)
		if speaker == "" && wire.SpeakerID != nil {
			speaker = fmt.Sprintf("Speaker %d", *wire.SpeakerID)
		}
		if speaker == "" {
			speaker = "Unknown speaker"
		}
		segments = append(segments, Segment{Speaker: speaker, Text: strings.TrimSpace(*wire.Text), OffsetSeconds: wire.Start})
	}
	if len(segments) == 0 {
		return Transcript{State: StateEmpty}, last - first
	}
	return Transcript{State: StateAvailable, Segments: segments}, last - first
}

func decodeOmiActions(fields map[string]jsontext.Value) ([]Action, Coverage, string) {
	raw, exists := fields["action_items"]
	if !exists {
		return []Action{}, CoverageUnavailable, reasonMissingField
	}
	var items []jsontext.Value
	if isNull(raw) || json.Unmarshal(raw, &items) != nil {
		return []Action{}, CoverageUnavailable, reasonInvalidSection
	}
	actions := make([]Action, 0, len(items))
	invalid := false
	for ordinal, item := range items {
		var wire struct {
			Description string `json:"description"`
			Completed   *bool  `json:"completed"`
			DueAt       string `json:"due_at"`
		}
		if json.Unmarshal(item, &wire) != nil || strings.TrimSpace(wire.Description) == "" {
			invalid = true
			continue
		}
		status := StatusUnknown
		if wire.Completed != nil {
			status = StatusPending
			if *wire.Completed {
				status = StatusCompleted
			}
		}
		actions = append(actions, Action{Ordinal: ordinal, Title: strings.TrimSpace(wire.Description), Status: status, DueDate: wire.DueAt, Origin: "structured", Locator: fmt.Sprintf("structured.action_items[%d]", ordinal)})
	}
	return actionResult(actions, invalid)
}
