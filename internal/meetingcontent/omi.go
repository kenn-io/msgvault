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
		if raw, ok := structured["sections"]; ok && !isNull(raw) {
			var sections []struct {
				Heading string `json:"heading"`
				Body    string `json:"body_markdown"`
			}
			if json.Unmarshal(raw, &sections) == nil {
				var notes []string
				for _, section := range sections {
					if text := strings.TrimSpace(section.Heading + "\n" + section.Body); text != "" {
						notes = append(notes, text)
					}
				}
				content.Notes = Section{State: StateEmpty}
				if len(notes) > 0 {
					content.Notes = Section{State: StateAvailable, Text: strings.Join(notes, "\n\n")}
				}
			} else {
				content.Notes = Section{State: StateUnavailable, Reason: reasonInvalidSection}
			}
		}
		content.Actions, content.ActionCoverage, content.ActionReason = decodeOmiActions(structured)
		var participants []struct {
			Name      string `json:"name"`
			Email     string `json:"email"`
			Source    string `json:"source"`
			IsAIAgent bool   `json:"is_ai_agent"`
		}
		if json.Unmarshal(structured["participants"], &participants) == nil {
			for _, person := range participants {
				if person.IsAIAgent {
					continue
				}
				name := strings.TrimSpace(person.Name)
				email := ""
				if person.Source == "roster" {
					email = normalizeExplicitEmail(person.Email)
				}
				if name != "" || email != "" {
					content.SourceParticipants = append(content.SourceParticipants, Participant{Name: name, Email: email, Role: "to"})
				}
			}
		}
	}
	content.Transcript = decodeOmiTranscript(fields)
	start, startOK := rawTime(fields["started_at"], false)
	end, endOK := rawTime(fields["finished_at"], false)
	if startOK && endOK && end.After(start) {
		setDuration(&content, end.Sub(start).Seconds(), DurationProvider)
	} else if span, ok := omiTranscriptSpan(fields["transcript_segments"]); ok {
		setDuration(&content, span, DurationTranscriptSpan)
	}
	// Speaker names are display evidence, not verified email identities.
	seen := map[string]bool{}
	for _, person := range content.SourceParticipants {
		seen[person.Name] = true
	}
	for _, s := range content.Transcript.Segments {
		if !seen[s.Speaker] {
			content.SourceParticipants = append(content.SourceParticipants, Participant{Name: s.Speaker, Role: "speaker"})
			seen[s.Speaker] = true
		}
	}
	return content
}

func decodeOmiTranscript(fields map[string]jsontext.Value) Transcript {
	raw, exists := fields["transcript_segments"]
	if !exists {
		return Transcript{State: StateUnavailable, Reason: reasonMissingField}
	}
	if isNull(raw) {
		return Transcript{State: StateUnavailable, Reason: reasonMissingField}
	}
	var items []jsontext.Value
	if json.Unmarshal(raw, &items) != nil {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	segments := make([]Segment, 0, len(items))
	for _, item := range items {
		var wire struct {
			Text      string   `json:"text"`
			Speaker   string   `json:"speaker"`
			Name      string   `json:"speaker_name"`
			SpeakerID *int     `json:"speaker_id"`
			IsUser    bool     `json:"is_user"`
			Start     *float64 `json:"start"`
			End       *float64 `json:"end"`
		}
		if json.Unmarshal(item, &wire) != nil || strings.TrimSpace(wire.Text) == "" || (wire.Start != nil && (!finite(*wire.Start) || *wire.Start < 0)) || (wire.End != nil && (!finite(*wire.End) || *wire.End < 0)) || (wire.Start != nil && wire.End != nil && *wire.End < *wire.Start) {
			return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		speaker := strings.TrimSpace(wire.Name)
		if speaker == "" && wire.IsUser {
			speaker = "You"
		}
		if speaker == "" {
			speaker = strings.TrimSpace(wire.Speaker)
		}
		if speaker == "" && wire.SpeakerID != nil {
			speaker = fmt.Sprintf("Speaker %d", *wire.SpeakerID)
		}
		if speaker == "" {
			speaker = "Unknown speaker"
		}
		segments = append(segments, Segment{Speaker: speaker, Text: strings.TrimSpace(wire.Text), OffsetSeconds: wire.Start})
	}
	if len(segments) == 0 {
		return Transcript{State: StateEmpty}
	}
	return Transcript{State: StateAvailable, Segments: segments}
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
			ID          string `json:"id"`
			Description string `json:"description"`
			Completed   *bool  `json:"completed"`
			DueAt       string `json:"due_at"`
			OwnerName   string `json:"owner_name"`
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
		actions = append(actions, Action{Ordinal: ordinal, SourceID: wire.ID, Title: strings.TrimSpace(wire.Description), Status: status, AssigneeName: wire.OwnerName, DueDate: wire.DueAt, Origin: "structured", Locator: fmt.Sprintf("structured.action_items[%d]", ordinal)})
	}
	return actionResult(actions, invalid)
}

// Omi offsets include segment ends; use the complete speech span rather than
// the distance between the first and last segment starts.
func omiTranscriptSpan(raw jsontext.Value) (float64, bool) {
	var segments []struct {
		Start *float64 `json:"start"`
		End   *float64 `json:"end"`
	}
	if json.Unmarshal(raw, &segments) != nil {
		return 0, false
	}
	var first, last float64
	found := false
	for _, segment := range segments {
		if segment.Start == nil || segment.End == nil || !finite(*segment.Start) || !finite(*segment.End) || *segment.Start < 0 || *segment.End < *segment.Start {
			continue
		}
		if !found {
			first, last, found = *segment.Start, *segment.End, true
		} else {
			first = min(first, *segment.Start)
			last = max(last, *segment.End)
		}
	}
	return last - first, found && last > first
}
