package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

func subtitleTiming(format string, raw []byte) ([]subtitleSegment, error) {
	content := Decode(format, raw, nil)
	if content.Transcript.State != StateAvailable || content.Transcript.Reason != "" || len(content.Transcript.Segments) == 0 {
		return nil, errors.New("a complete timed transcript is unavailable")
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil {
		return nil, errors.New("invalid raw transcript")
	}
	cues := make([]subtitleSegment, 0, len(content.Transcript.Segments))
	switch format {
	case "omi_json":
		origin, ok := rawTime(fields["started_at"], false)
		if !ok || origin.IsZero() {
			return nil, errors.New("recording start is unknown")
		}
		var items []struct {
			Text      string   `json:"text"`
			Name      string   `json:"speaker_name"`
			SpeakerID *int     `json:"speaker_id"`
			Start     *float64 `json:"start"`
			End       *float64 `json:"end"`
		}
		if json.Unmarshal(fields["transcript_segments"], &items) != nil {
			return nil, errors.New("invalid transcript")
		}
		for _, item := range items {
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			if item.Start == nil || item.End == nil {
				return nil, errors.New("transcript needs start and end times")
			}
			i := len(cues)
			if i >= len(content.Transcript.Segments) {
				return nil, errors.New("transcript changed")
			}
			seg := content.Transcript.Segments[i]
			speaker := seg.Speaker
			if strings.TrimSpace(item.Name) == "" && item.SpeakerID == nil {
				speaker = ""
			}
			cues = append(cues, subtitleSegment{speaker, seg.Text, *item.Start, *item.End})
		}
	case "pocket_json":
		origin, ok := rawTime(fields["recorded_at"], false)
		if !ok || origin.IsZero() {
			var recording map[string]jsontext.Value
			_ = json.Unmarshal(fields["recording"], &recording)
			origin, ok = rawTime(recording["recording_at"], false)
		}
		if !ok || origin.IsZero() {
			return nil, errors.New("recording start is unknown")
		}
		var recording struct {
			TranscriptError string `json:"transcript_error"`
			Transcript      struct {
				Segments []struct {
					Speaker      string   `json:"speaker"`
					SpeakerName  string   `json:"speakerName"`
					Text         string   `json:"text"`
					OriginalText string   `json:"originalText"`
					Start        *float64 `json:"start"`
					End          *float64 `json:"end"`
				} `json:"segments"`
			} `json:"transcript"`
		}
		if json.Unmarshal(fields["recording"], &recording) != nil || strings.TrimSpace(recording.TranscriptError) != "" || len(recording.Transcript.Segments) != len(content.Transcript.Segments) {
			return nil, errors.New("retained transcript timing is unavailable")
		}
		for i, item := range recording.Transcript.Segments {
			seg := content.Transcript.Segments[i]
			text := item.OriginalText
			if strings.TrimSpace(text) == "" {
				text = item.Text
			}
			speaker := strings.TrimSpace(item.SpeakerName)
			if speaker == "" {
				speaker = strings.TrimSpace(item.Speaker)
			}
			if text != seg.Text || speaker != seg.Speaker || item.Start == nil || item.End == nil || seg.OffsetSeconds == nil || *item.Start != *seg.OffsetSeconds {
				return nil, errors.New("retained transcript timing does not match archived transcript")
			}
			cues = append(cues, subtitleSegment{seg.Speaker, seg.Text, *item.Start, *item.End})
		}
	case "twenty_json":
		var recording map[string]jsontext.Value
		if json.Unmarshal(fields["recording"], &recording) != nil {
			return nil, errors.New("invalid recording")
		}
		entries, ok := twentyEntries(recording["transcript"])
		if !ok {
			return nil, errors.New("invalid transcript")
		}
		origin, hasOrigin := rawTime(recording["startedAt"], false)
		if !hasOrigin || origin.IsZero() {
			return nil, errors.New("recording start is unknown")
		}
		for i, entry := range entries {
			startStamp, endStamp := entry.words[0].start, entry.words[len(entry.words)-1].end
			start, hasStart := twentyOffset(startStamp)
			end, hasEnd := twentyOffset(endStamp)
			if !hasStart || !hasEnd {
				startTime, startOK := rawTime(startStamp["absolute"], false)
				endTime, endOK := rawTime(endStamp["absolute"], false)
				if !startOK || !endOK {
					return nil, errors.New("transcript needs matching start and end times")
				}
				var err error
				start, err = subtitleSeconds(origin, startTime)
				if err != nil {
					return nil, err
				}
				end, err = subtitleSeconds(origin, endTime)
				if err != nil {
					return nil, err
				}
			}
			seg := content.Transcript.Segments[i]
			cues = append(cues, subtitleSegment{entry.speaker, seg.Text, start, end})
		}
	default:
		return nil, fmt.Errorf("recording start is unknown or timing is unavailable for %s", format)
	}
	if len(cues) != len(content.Transcript.Segments) {
		return nil, errors.New("transcript timing is incomplete")
	}
	return cues, nil
}
