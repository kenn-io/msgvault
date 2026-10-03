package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Twilio snapshots retain provider evidence alongside normalized, selected
// speech. The provider does not supply summaries or actions through these reads.
func decodeTwilio(fields map[string]jsontext.Value) Content {
	content := decodeGeneric(fields)
	content.Summary = Section{State: StateUnsupported}
	content.Notes = Section{State: StateUnsupported}
	content.Actions = []Action{}
	content.ActionCoverage = CoverageUnsupported
	content.ActionReason = "no_structured_actions"
	// A call's duration is authoritative even when separately scoped Relay
	// sessions or Orchestrator communications have no synchronized offsets.
	content.DurationSeconds = nil
	content.DurationBasis = ""
	if seconds, ok := rawPositiveFloat(fields["duration_seconds"]); ok {
		setDuration(&content, seconds, DurationProvider)
	}
	if content.Transcript.State == StateAvailable {
		var wire []struct {
			StartedAt jsontext.Value `json:"started_at"`
		}
		if json.Unmarshal(fields["transcript_segments"], &wire) == nil {
			for i, segment := range wire {
				if i >= len(content.Transcript.Segments) {
					break
				}
				if at, ok := rawTime(segment.StartedAt, false); ok {
					content.Transcript.Segments[i].StartedAt = &at
				}
			}
		}
	}
	return content
}
