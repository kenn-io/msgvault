package meetingcontent

import (
	"encoding/json"
	"fmt"
	"strings"
)

func decodeGeneric(fields map[string]json.RawMessage) Content {
	content := baseRecognizedContent()
	content.Summary = decodeStringAliases(fields, "summary_markdown", "summary_text")
	content.Notes = Section{State: StateUnsupported}
	content.Transcript = decodeGenericTranscript(fields)
	content.Actions, content.ActionCoverage, content.ActionReason = decodeGenericActions(fields)
	content.SourceParticipants = providerParticipants(fields)
	start, startOK := rawTime(fields["started_at"], false)
	end, endOK := rawTime(fields["ended_at"], false)
	if startOK && endOK && end.After(start) {
		setDuration(&content, end.Sub(start).Seconds(), DurationProvider)
	} else if value, ok := transcriptDuration(content.Transcript.Segments); ok {
		setDuration(&content, value, DurationTranscriptSpan)
	}
	return content
}

func decodeGenericTranscript(fields map[string]json.RawMessage) Transcript {
	text, textOK := optionalString(fields["transcript"])
	if !textOK {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	segmentsRaw, segmentsPresent := fields["transcript_segments"]
	segments := []Segment{}
	if segmentsPresent {
		if isNull(segmentsRaw) {
			return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		var items []json.RawMessage
		if json.Unmarshal(segmentsRaw, &items) != nil {
			return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		for _, item := range items {
			var wire struct {
				Speaker json.RawMessage `json:"speaker"`
				Text    json.RawMessage `json:"text"`
				Offset  json.RawMessage `json:"offset_seconds"`
			}
			if json.Unmarshal(item, &wire) != nil {
				return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
			}
			speaker, speakerOK := optionalString(wire.Speaker)
			utterance, utteranceOK := optionalString(wire.Text)
			if !speakerOK || !utteranceOK || strings.TrimSpace(utterance) == "" {
				return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
			}
			segment := Segment{Speaker: strings.TrimSpace(speaker), Text: strings.TrimSpace(utterance)}
			if len(wire.Offset) > 0 && !isNull(wire.Offset) {
				offset, valid := rawFiniteFloat(wire.Offset)
				if !valid || offset < 0 {
					return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
				}
				segment.OffsetSeconds = &offset
			}
			segments = append(segments, segment)
		}
	}
	text = strings.TrimSpace(text)
	if text != "" || len(segments) > 0 {
		return Transcript{State: StateAvailable, Text: text, Segments: segments}
	}
	if _, textPresent := fields["transcript"]; textPresent || segmentsPresent {
		return Transcript{State: StateEmpty}
	}
	return Transcript{State: StateUnavailable, Reason: reasonMissingField}
}

func decodeGenericActions(fields map[string]json.RawMessage) ([]Action, Coverage, string) {
	raw, ok := fields["action_items"]
	if !ok {
		return []Action{}, CoverageUnsupported, "no_structured_actions"
	}
	if isNull(raw) {
		return []Action{}, CoverageUnavailable, reasonInvalidSection
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return []Action{}, CoverageUnavailable, reasonInvalidSection
	}
	actions := make([]Action, 0, len(items))
	invalid := false
	for ordinal, item := range items {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil || fields == nil {
			invalid = true
			continue
		}
		sourceID, sourceOK := optionalString(fields["source_id"])
		title, titleOK := optionalString(fields["title"])
		description, descriptionOK := optionalString(fields["description"])
		assigneeName, nameOK := optionalString(fields["assignee_name"])
		assigneeEmail, emailOK := optionalString(fields["assignee_email"])
		status, statusOK := optionalString(fields["status"])
		dueDate, dueOK := optionalString(fields["due_date"])
		if !sourceOK || !titleOK || strings.TrimSpace(title) == "" || !descriptionOK || !nameOK || !emailOK || !statusOK || !dueOK {
			invalid = true
			continue
		}
		actions = append(actions, Action{
			Ordinal: ordinal, SourceID: strings.TrimSpace(sourceID), Title: strings.TrimSpace(title),
			Description: strings.TrimSpace(description), AssigneeName: strings.TrimSpace(assigneeName),
			AssigneeEmail: normalizeExplicitEmail(assigneeEmail), Status: NormalizeStatus(status),
			SourceStatus: strings.TrimSpace(status), DueDate: strings.TrimSpace(dueDate),
			Origin: "structured", Locator: fmt.Sprintf("action_items[%d]", ordinal),
		})
	}
	return actionResult(actions, invalid)
}
