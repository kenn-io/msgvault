package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

func decodePlaud(fields map[string]jsontext.Value) Content {
	version, ok := rawInteger(fields["schema_version"])
	if len(fields["schema_version"]) > 0 && (!ok || (version != 1 && version != 2 && version != 3)) {
		return unavailableContent("unsupported_schema")
	}
	content := decodeGeneric(fields)
	content.Notes = decodeStringField(fields, "notes")
	if version == 0 {
		content.Transcript.Reason = "plaud_units_unknown"
	}
	for i := range content.Transcript.Segments {
		segment := &content.Transcript.Segments[i]
		if version == 0 {
			segment.OffsetSeconds, segment.EndOffsetSeconds = nil, nil
		} else if version < 3 {
			if segment.OffsetSeconds != nil {
				if version == 1 && *segment.OffsetSeconds == 0 {
					segment.OffsetSeconds = nil
				} else {
					*segment.OffsetSeconds /= 1000
				}
			}
			if segment.EndOffsetSeconds != nil {
				if *segment.EndOffsetSeconds == 0 {
					segment.EndOffsetSeconds = nil
				} else {
					*segment.EndOffsetSeconds /= 1000
				}
			}
		}
	}
	if content.DurationBasis == DurationTranscriptSpan {
		content.DurationSeconds, content.DurationBasis = nil, ""
		if version != 0 {
			if duration, valid := transcriptDuration(content.Transcript.Segments); valid {
				setDuration(&content, duration, DurationTranscriptSpan)
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
