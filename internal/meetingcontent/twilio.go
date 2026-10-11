package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// reasonProviderRefused marks a transcript Twilio refused to serve (401/403/404).
const reasonProviderRefused = "provider_refused"

// Twilio snapshots retain provider evidence alongside normalized, selected
// speech. The provider does not supply summaries or actions through these reads.
func decodeTwilio(fields map[string]jsontext.Value) Content {
	content := decodeGeneric(fields)
	content.Summary = Section{State: StateUnsupported}
	if content.Transcript.State == StateUnavailable && content.Transcript.Reason == reasonMissingField && twilioTranscriptRefused(fields["evidence"]) {
		content.Transcript.Reason = reasonProviderRefused
	}
	// The call's duration is authoritative over transcript offsets.
	content.DurationSeconds = nil
	content.DurationBasis = ""
	if seconds, ok := rawPositiveFloat(fields["duration_seconds"]); ok {
		setDuration(&content, seconds, DurationProvider)
	}
	return content
}

// twilioTranscriptRefused reports a stored note that Twilio refused a
// transcript read.
func twilioTranscriptRefused(raw jsontext.Value) bool {
	var evidence struct {
		Diagnostics []string `json:"diagnostics"`
	}
	if json.Unmarshal(raw, &evidence) != nil {
		return false
	}
	for _, diagnostic := range evidence.Diagnostics {
		if strings.Contains(diagnostic, "coverage unavailable (HTTP ") {
			return true
		}
	}
	return false
}
