package meetingcontent

import (
	"encoding/json"
	"strings"
	"time"
)

func decodeGranola(fields map[string]json.RawMessage) Content {
	content := baseRecognizedContent()
	content.Summary = decodeStringAliases(fields, "summary_markdown", "summary_text")
	content.Notes = Section{State: StateUnsupported}
	content.ActionCoverage = CoverageUnsupported
	content.ActionReason = "no_structured_actions"

	segments, transcript := decodeGranolaTranscript(fields)
	content.Transcript = transcript
	if transcript.State == StateAvailable {
		content.Transcript.Segments = segments
	}
	content.SourceParticipants = granolaParticipants(fields)

	if calendarRaw, ok := fields["calendar_event"]; ok && !isNull(calendarRaw) {
		var calendar map[string]json.RawMessage
		if json.Unmarshal(calendarRaw, &calendar) == nil {
			start, startOK := rawTime(calendar["scheduled_start_time"], false)
			end, endOK := rawTime(calendar["scheduled_end_time"], false)
			if startOK && endOK && end.After(start) {
				setDuration(&content, end.Sub(start).Seconds(), DurationScheduled)
				return content
			}
		}
	}
	var starts, ends []time.Time
	for _, segment := range segments {
		if segment.StartedAt != nil {
			starts = append(starts, *segment.StartedAt)
		}
		if segment.EndedAt != nil {
			ends = append(ends, *segment.EndedAt)
		}
	}
	if start, ok := minTime(starts); ok {
		if end, endOK := maxTime(ends); endOK && end.After(start) {
			setDuration(&content, end.Sub(start).Seconds(), DurationTranscriptSpan)
		}
	}
	return content
}

func decodeGranolaTranscript(fields map[string]json.RawMessage) ([]Segment, Transcript) {
	raw, ok := fields["transcript"]
	if !ok {
		return nil, Transcript{State: StateUnavailable, Reason: reasonMissingField}
	}
	if isNull(raw) {
		return nil, Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	segments := make([]Segment, 0, len(items))
	for _, item := range items {
		var wire struct {
			Speaker json.RawMessage `json:"speaker"`
			Text    json.RawMessage `json:"text"`
			Start   json.RawMessage `json:"start_time"`
			End     json.RawMessage `json:"end_time"`
		}
		if json.Unmarshal(item, &wire) != nil {
			return nil, Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		text, valid := optionalString(wire.Text)
		if !valid {
			return nil, Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		var speaker struct {
			Source           string `json:"source"`
			DiarizationLabel string `json:"diarization_label"`
			Name             string `json:"name"`
		}
		if len(wire.Speaker) > 0 && !isNull(wire.Speaker) && json.Unmarshal(wire.Speaker, &speaker) != nil {
			return nil, Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		segment := Segment{Speaker: granolaSpeaker(speaker.Name, speaker.DiarizationLabel, speaker.Source), Text: strings.TrimSpace(text)}
		if len(wire.Start) > 0 && !isNull(wire.Start) {
			started, validTime := rawTime(wire.Start, false)
			if !validTime {
				return nil, Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
			}
			segment.StartedAt = &started
		}
		if len(wire.End) > 0 && !isNull(wire.End) {
			ended, validTime := rawTime(wire.End, false)
			if !validTime {
				return nil, Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
			}
			segment.EndedAt = &ended
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		return []Segment{}, Transcript{State: StateEmpty}
	}
	return segments, Transcript{State: StateAvailable, Segments: segments}
}

func granolaSpeaker(name, diarization, source string) string {
	if value := strings.TrimSpace(name); value != "" {
		return value
	}
	if value := strings.TrimSpace(diarization); value != "" {
		return value
	}
	if source == "microphone" {
		return "Me"
	}
	return "Them"
}

func granolaParticipants(fields map[string]json.RawMessage) []Participant {
	var owner personWire
	_ = json.Unmarshal(fields["owner"], &owner)
	var calendar struct {
		Organiser string `json:"organiser"`
		Invitees  []struct {
			Email string `json:"email"`
		} `json:"invitees"`
	}
	_ = json.Unmarshal(fields["calendar_event"], &calendar)
	organizer := owner
	if email := strings.TrimSpace(calendar.Organiser); email != "" {
		organizer = personWire{Email: email}
		if strings.EqualFold(owner.Email, email) {
			organizer.Name = owner.Name
		}
	}
	var attendees []personWire
	_ = json.Unmarshal(fields["attendees"], &attendees)
	if len(attendees) == 0 {
		for _, invitee := range calendar.Invitees {
			attendees = append(attendees, personWire{Email: invitee.Email})
		}
	}
	for _, attendee := range attendees {
		if organizer.Name == "" && strings.EqualFold(attendee.Email, organizer.Email) {
			organizer.Name = attendee.Name
		}
	}
	return participantsFromPeople(&organizer, attendees)
}
