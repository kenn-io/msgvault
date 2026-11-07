package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

func decodePlaud(fields map[string]jsontext.Value) Content {
	version, ok := rawInteger(fields["schema_version"])
	if len(fields["schema_version"]) > 0 && (!ok || (version != 1 && version != 2)) {
		return unavailableContent("unsupported_schema")
	}
	content := decodeGeneric(fields)
	content.Notes = decodeStringField(fields, "notes")
	if version != 2 {
		for i := range content.Transcript.Segments {
			segment := &content.Transcript.Segments[i]
			if segment.OffsetSeconds != nil && *segment.OffsetSeconds == 0 {
				segment.OffsetSeconds = nil
			}
		}
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
