package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

func decodePlaud(fields map[string]jsontext.Value) Content {
	content := decodeGeneric(fields)
	content.Notes = decodeStringField(fields, "notes")
	if seconds, ok := rawFiniteFloat(fields["duration_seconds"]); ok {
		setDuration(&content, seconds, DurationProvider)
	}
	var participants []Participant
	if json.Unmarshal(fields["source_participants"], &participants) == nil {
		// Speaker labels are display evidence, never verified email identities.
		for _, p := range participants {
			if p.Name != "" {
				content.SourceParticipants = append(content.SourceParticipants, Participant{Name: p.Name, Role: "speaker"})
			}
		}
	}
	return content
}
