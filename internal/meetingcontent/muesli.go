package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// decodeMuesli reads msgvault's muesli_json evidence. Muesli has no
// structured action items, and its transcript is plain text whose line stamps
// are local wall-clock times, so the transcript stays unsegmented.
func decodeMuesli(fields map[string]jsontext.Value) Content {
	var meeting map[string]jsontext.Value
	raw, ok := fields["meeting"]
	if !ok || isNull(raw) || json.Unmarshal(raw, &meeting) != nil || meeting == nil {
		return unavailableContent(reasonMissingField)
	}
	content := baseRecognizedContent()
	// Fallback and failure notices are not summaries; the transcript and the
	// raw evidence still carry their text.
	content.Summary = Section{State: StateEmpty}
	if state, _ := optionalString(meeting["notes_state"]); state == "structured_notes" {
		content.Summary = optionalSection(meeting, "formatted_notes")
	}
	content.Notes = optionalSection(meeting, "manual_notes")
	content.Transcript = Transcript{State: StateEmpty}
	if text, _ := optionalString(meeting["raw_transcript"]); strings.TrimSpace(text) != "" {
		content.Transcript = Transcript{State: StateAvailable, Text: strings.TrimSpace(text)}
	}
	content.ActionCoverage = CoverageUnsupported
	content.ActionReason = "no_structured_actions"

	// Archived recipients use the Contacts email (or phone) when Muesli has
	// none, so the source participants must too, or packets list them twice.
	var participants []struct {
		personWire

		Emails []string `json:"emails"`
		Phones []string `json:"phones"`
	}
	_ = json.Unmarshal(fields["participants"], &participants)
	people := make([]personWire, 0, len(participants))
	for _, participant := range participants {
		person := participant.personWire
		if strings.TrimSpace(person.Email) == "" && len(participant.Emails) > 0 {
			person.Email = participant.Emails[0]
		}
		if strings.TrimSpace(person.Email) == "" && strings.TrimSpace(person.Phone) == "" && len(participant.Phones) > 0 {
			person.Phone = participant.Phones[0]
		}
		people = append(people, person)
	}
	content.SourceParticipants = participantsFromPeople(nil, people)

	if seconds, ok := rawPositiveFloat(meeting["duration_seconds"]); ok {
		setDuration(&content, seconds, DurationProvider)
	} else {
		start, startOK := rawTime(meeting["start_time"], false)
		end, endOK := rawTime(meeting["end_time"], false)
		if startOK && endOK && end.After(start) {
			setDuration(&content, end.Sub(start).Seconds(), DurationProvider)
		}
	}
	return content
}

// optionalSection treats an omitted field as empty: muesli_json omits blank
// text fields rather than writing empty strings.
func optionalSection(fields map[string]jsontext.Value, key string) Section {
	if _, present := fields[key]; !present {
		return Section{State: StateEmpty}
	}
	return decodeStringField(fields, key)
}
