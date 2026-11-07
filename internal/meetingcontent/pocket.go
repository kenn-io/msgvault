package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
)

// Pocket evidence is normalized at ingestion and versioned independently of
// upstream responses. Reject unknown versions rather than guessing their shape.
func decodePocket(fields map[string]jsontext.Value) Content {
	var version int
	var content Content
	if json.Unmarshal(fields["schema_version"], &version) != nil || (version != 1 && version != 2) || json.Unmarshal(fields["content"], &content) != nil {
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
	return recoverPocketTranscript(fields["recording"], content)
}

// NormalizePocketTranscript reads Pocket's retained transcript segment object.
func NormalizePocketTranscript(raw jsontext.Value) (Transcript, []Participant, error) {
	// The REST schema wraps segments in an object. speaker is a raw
	// diarization label such as SPEAKER_00; speakerName is present only
	// when Pocket resolved that label. text is translated when a
	// translation exists, so the archive keeps originalText.
	var wireTranscript struct {
		Segments []struct {
			Speaker      string   `json:"speaker"`
			SpeakerName  string   `json:"speakerName"`
			Text         string   `json:"text"`
			OriginalText string   `json:"originalText"`
			Start        *float64 `json:"start"`
			End          *float64 `json:"end"`
		} `json:"segments"`
	}
	if json.Unmarshal(raw, &wireTranscript) != nil || wireTranscript.Segments == nil {
		return Transcript{}, nil, errors.New("incompatible transcript")
	}
	segments := wireTranscript.Segments
	transcript := Transcript{State: StateEmpty, Segments: []Segment{}}
	var participants []Participant
	seen := map[string]bool{}
	for _, item := range segments {
		text := item.OriginalText
		if strings.TrimSpace(text) == "" {
			text = item.Text
		}
		if strings.TrimSpace(text) == "" || invalidPocketSeconds(item.Start) || invalidPocketSeconds(item.End) || item.Start != nil && item.End != nil && *item.End < *item.Start {
			return Transcript{}, nil, errors.New("incompatible transcript segment")
		}
		name := strings.TrimSpace(item.SpeakerName)
		speaker := name
		if speaker == "" {
			speaker = strings.TrimSpace(item.Speaker)
		}
		transcript.Segments = append(transcript.Segments, Segment{Speaker: speaker, Text: text, OffsetSeconds: item.Start, EndOffsetSeconds: item.End})
		// Unresolved diarization labels are not people.
		if name != "" && !seen[name] {
			participants = append(participants, Participant{Name: name, Role: "speaker"})
			seen[name] = true
		}
	}
	if len(segments) > 0 {
		transcript.State = StateAvailable
	}
	return transcript, participants, nil
}

func invalidPocketSeconds(value *float64) bool {
	return value != nil && (*value < 0 || !finite(*value))
}

func recoverPocketTranscript(raw jsontext.Value, content Content) Content {
	var recording struct {
		Transcript jsontext.Value `json:"transcript"`
		Error      string         `json:"transcript_error"`
	}
	if json.Unmarshal(raw, &recording) != nil || strings.TrimSpace(recording.Error) != "" || len(recording.Transcript) == 0 || isNull(recording.Transcript) {
		return content
	}
	candidate, _, err := NormalizePocketTranscript(recording.Transcript)
	if err != nil || len(candidate.Segments) != len(content.Transcript.Segments) {
		return content
	}
	completed := content.Transcript
	completed.Segments = slices.Clone(completed.Segments)
	for i := range completed.Segments {
		if completed.Segments[i].EndOffsetSeconds == nil {
			completed.Segments[i].EndOffsetSeconds = candidate.Segments[i].EndOffsetSeconds
		}
	}
	if reflect.DeepEqual(completed, candidate) {
		content.Transcript = completed
	}
	return content
}
