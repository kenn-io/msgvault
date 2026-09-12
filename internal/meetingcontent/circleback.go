package meetingcontent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func decodeCircleback(fields map[string]json.RawMessage) Content {
	meetingRaw, ok := fields["meeting"]
	if !ok {
		return unavailableContent(reasonMissingField)
	}
	if isNull(meetingRaw) {
		return unavailableContent(reasonInvalidSection)
	}
	var meeting map[string]json.RawMessage
	if json.Unmarshal(meetingRaw, &meeting) != nil || meeting == nil {
		return unavailableContent(reasonInvalidSection)
	}
	content := baseRecognizedContent()
	content.Summary = decodeCirclebackStringAliases(meeting, "notes", "summary")
	content.Notes = Section{State: StateUnsupported}
	content.Actions, content.ActionCoverage, content.ActionReason = decodeCirclebackActions(meeting)
	content.SourceParticipants = providerParticipants(meeting)
	content.Transcript = decodeCirclebackTranscript(fields["transcript"])

	for _, key := range []string{"durationSeconds", "duration"} {
		if value, valid := rawPositiveFloat(meeting[key]); valid {
			setDuration(&content, value, DurationProvider)
			return content
		}
	}
	for _, startKey := range []string{"startTime", "date"} {
		start, startOK := rawTime(meeting[startKey], true)
		end, endOK := rawTime(meeting["endTime"], true)
		if startOK && endOK && end.After(start) {
			setDuration(&content, end.Sub(start).Seconds(), DurationScheduled)
			return content
		}
	}
	if value, ok := transcriptDuration(content.Transcript.Segments); ok {
		setDuration(&content, value, DurationTranscriptSpan)
	}
	return content
}

func decodeCirclebackActions(meeting map[string]json.RawMessage) ([]Action, Coverage, string) {
	raw, ok := meeting["actionItems"]
	if !ok {
		return []Action{}, CoverageUnavailable, reasonMissingField
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
		title, titleOK := circlebackFirstValidString(fields, "title", "name", "description")
		if !titleOK || strings.TrimSpace(title) == "" {
			invalid = true
			continue
		}
		description, descriptionOK := circlebackOptionalString(fields["description"])
		status, statusOK := circlebackOptionalString(fields["status"])
		dueDate, dueOK := circlebackOptionalString(fields["dueDate"])
		assigneeName, assigneeEmail, assigneeOK := circlebackAssignee(fields["assignee"])
		if !descriptionOK || !statusOK || !dueOK || !assigneeOK {
			invalid = true
			continue
		}
		actions = append(actions, Action{
			Ordinal: ordinal, Title: strings.TrimSpace(title), Description: strings.TrimSpace(description),
			AssigneeName: strings.TrimSpace(assigneeName), AssigneeEmail: normalizeExplicitEmail(assigneeEmail),
			Status: NormalizeStatus(status), SourceStatus: strings.TrimSpace(status), DueDate: strings.TrimSpace(dueDate),
			Origin: "structured", Locator: fmt.Sprintf("meeting.actionItems[%d]", ordinal),
		})
	}
	return actionResult(actions, invalid)
}

func circlebackAssignee(raw json.RawMessage) (string, string, bool) {
	if len(raw) == 0 || isNull(raw) {
		return "", "", true
	}
	var label string
	if json.Unmarshal(raw, &label) == nil {
		return label, "", true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", "", false
	}
	name, nameOK := circlebackFirstValidString(fields, "name", "displayName", "email")
	email, emailOK := circlebackOptionalString(fields["email"])
	return name, email, nameOK && emailOK
}

func decodeCirclebackTranscript(raw json.RawMessage) Transcript {
	if len(raw) == 0 {
		return Transcript{State: StateUnavailable, Reason: reasonMissingField}
	}
	if isNull(raw) {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	text, textOK := circlebackOptionalString(fields["text"])
	if !textOK {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	entriesRaw, entriesPresent := fields["transcript"]
	var segments []Segment
	if entriesPresent {
		var items []json.RawMessage
		if !isNull(entriesRaw) && json.Unmarshal(entriesRaw, &items) != nil {
			return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		segments = make([]Segment, 0, len(items))
		for _, item := range items {
			segment, recognized, valid := decodeCirclebackSegment(item)
			if !valid {
				return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
			}
			if recognized && strings.TrimSpace(segment.Text) != "" {
				segments = append(segments, segment)
			}
		}
	}
	text = strings.TrimSpace(text)
	if text == "" && len(segments) == 0 {
		if _, textPresent := fields["text"]; textPresent || entriesPresent {
			return Transcript{State: StateEmpty}
		}
		return Transcript{State: StateUnavailable, Reason: reasonMissingField}
	}
	return Transcript{State: StateAvailable, Text: text, Segments: segments}
}

func decodeCirclebackSegment(raw json.RawMessage) (Segment, bool, bool) {
	if isNull(raw) {
		return Segment{}, false, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Segment{}, false, false
	}
	text, textOK := circlebackFirstValidString(fields, "text", "content", "words")
	if !textOK {
		return Segment{}, false, false
	}
	_, recognized := firstPresent(fields, "text", "content", "words")
	if !recognized {
		knownMetadata := map[string]bool{
			"speaker": true, "speakerName": true, "timestamp": true,
			"start": true, "startTimestamp": true, "time": true,
		}
		for key, value := range fields {
			if !knownMetadata[key] && rawJSONNonBlank(value) {
				return Segment{}, false, false
			}
		}
	}
	speaker, speakerOK := circlebackFirstValidString(fields, "speaker", "speakerName")
	if !speakerOK {
		return Segment{}, false, false
	}
	segment := Segment{Speaker: strings.TrimSpace(speaker), Text: strings.TrimSpace(text)}
	for _, key := range []string{"timestamp", "start", "startTimestamp", "time"} {
		value, ok := fields[key]
		if !ok || isNull(value) {
			continue
		}
		if number, valid := rawFiniteFloat(value); valid {
			segment.OffsetSeconds = &number
			break
		}
		if parsed, valid := rawTime(value, true); valid {
			segment.StartedAt = &parsed
			break
		}
	}
	return segment, recognized, true
}

func decodeCirclebackStringAliases(fields map[string]json.RawMessage, keys ...string) Section {
	present := false
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		present = true
		value, valid := circlebackOptionalString(raw)
		if !valid {
			return Section{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		if value = strings.TrimSpace(value); value != "" {
			return Section{State: StateAvailable, Text: value}
		}
	}
	if present {
		return Section{State: StateEmpty}
	}
	return Section{State: StateUnavailable, Reason: reasonMissingField}
}

func circlebackOptionalString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isNull(raw) {
		return "", true
	}
	return optionalString(raw)
}

func circlebackFirstValidString(fields map[string]json.RawMessage, keys ...string) (string, bool) {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		value, valid := circlebackOptionalString(raw)
		if !valid {
			return "", false
		}
		if strings.TrimSpace(value) != "" {
			return value, true
		}
	}
	return "", true
}

func rawJSONNonBlank(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isNull(trimmed) || bytes.Equal(trimmed, []byte(`""`)) ||
		bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("{}")) {
		return false
	}
	if trimmed[0] == '"' {
		var value string
		return json.Unmarshal(trimmed, &value) != nil || strings.TrimSpace(value) != ""
	}
	return true
}
