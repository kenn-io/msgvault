package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// Pocket evidence is normalized at ingestion and versioned independently of
// upstream responses. Reject unknown versions rather than guessing their shape.
func decodePocket(fields map[string]jsontext.Value) Content {
	var version int
	var content Content
	if json.Unmarshal(fields["schema_version"], &version) != nil || version != 1 || json.Unmarshal(fields["content"], &content) != nil {
		return unavailableContent("invalid_raw")
	}
	validState := func(s State) bool {
		return s == StateAvailable || s == StateEmpty || s == StateUnavailable || s == StateUnsupported
	}
	if !validState(content.Summary.State) || !validState(content.Notes.State) || !validState(content.Transcript.State) {
		return unavailableContent("invalid_raw")
	}
	switch content.ActionCoverage {
	case CoverageAvailable, CoveragePartial, CoverageUnavailable, CoverageUnsupported:
	default:
		return unavailableContent("invalid_raw")
	}
	if content.DurationSeconds != nil && (!finite(*content.DurationSeconds) || *content.DurationSeconds < 0 || content.DurationBasis != DurationProvider) {
		return unavailableContent("invalid_raw")
	}
	for _, seg := range content.Transcript.Segments {
		if strings.TrimSpace(seg.Text) == "" || seg.OffsetSeconds != nil && (!finite(*seg.OffsetSeconds) || *seg.OffsetSeconds < 0) {
			return unavailableContent("invalid_raw")
		}
	}
	for _, action := range content.Actions {
		if strings.TrimSpace(action.Title) == "" {
			return unavailableContent("invalid_raw")
		}
		switch action.Status {
		case StatusPending, StatusCompleted, StatusCancelled, StatusUnknown:
		default:
			return unavailableContent("invalid_raw")
		}
	}
	if content.Actions == nil {
		content.Actions = []Action{}
	}
	return content
}
