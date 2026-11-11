package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

func subtitleTiming(format string, raw []byte) ([]subtitleSegment, error) {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errors.New("invalid raw transcript")
	}
	if format == "twilio_call_json" {
		return twilioSubtitleTiming(fields)
	}
	var cues []subtitleSegment
	switch format {
	case "omi_json":
		origin, ok := rawTime(fields["started_at"], false)
		if !ok || origin.IsZero() {
			return nil, errors.New("recording start is unknown")
		}
		var items []struct {
			Text      *string  `json:"text"`
			Name      string   `json:"speaker_name"`
			SpeakerID *int     `json:"speaker_id"`
			Start     *float64 `json:"start"`
			End       *float64 `json:"end"`
		}
		if json.Unmarshal(fields["transcript_segments"], &items) != nil {
			return nil, errors.New("invalid transcript")
		}
		for _, item := range items {
			if item.Text == nil {
				return nil, errors.New("invalid transcript")
			}
			if strings.TrimSpace(*item.Text) == "" {
				continue
			}
			if item.Start == nil || item.End == nil {
				return nil, errors.New("transcript needs start and end times")
			}
			speaker := strings.TrimSpace(item.Name)
			if speaker == "" && item.SpeakerID != nil {
				speaker = fmt.Sprintf("Speaker %d", *item.SpeakerID)
			}
			cues = append(cues, subtitleSegment{speaker, *item.Text, *item.Start, *item.End})
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
		transcript := decodePocket(fields).Transcript
		if transcript.State != StateAvailable || transcript.Reason != "" || len(transcript.Segments) == 0 {
			return nil, errors.New("a complete timed transcript is unavailable")
		}
		var recording struct {
			TranscriptError string `json:"transcript_error"`
			Transcript      *struct {
				Segments []struct {
					Start *float64 `json:"start"`
					End   *float64 `json:"end"`
				} `json:"segments"`
			} `json:"transcript"`
		}
		if json.Unmarshal(fields["recording"], &recording) != nil || strings.TrimSpace(recording.TranscriptError) != "" || recording.Transcript == nil || len(recording.Transcript.Segments) != len(transcript.Segments) {
			return nil, errors.New("retained transcript timing is unavailable")
		}
		for i, item := range recording.Transcript.Segments {
			seg := transcript.Segments[i]
			if item.Start == nil || item.End == nil || seg.OffsetSeconds == nil || *item.Start != *seg.OffsetSeconds {
				return nil, errors.New("retained transcript timing does not match archived transcript")
			}
			cues = append(cues, subtitleSegment{seg.Speaker, seg.Text, *item.Start, *item.End})
		}

	case "twenty_json":
		version, ok := rawInteger(fields["schema_version"])
		var recording map[string]jsontext.Value
		if !ok || version != 1 || json.Unmarshal(fields["recording"], &recording) != nil || recording == nil {
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
		for _, entry := range entries {
			startStamp, endStamp := entry.words[0].start, entry.words[len(entry.words)-1].end
			start, hasStart := twentyOffset(startStamp)
			end, hasEnd := twentyOffset(endStamp)
			relative := hasStart && hasEnd
			if !hasStart || !hasEnd {
				startTime, startOK := rawTime(startStamp["absolute"], false)
				endTime, endOK := rawTime(endStamp["absolute"], false)
				if !startOK || !endOK {
					return nil, errors.New("transcript needs matching start and end times")
				}
				start, end = startTime.Sub(origin).Seconds(), endTime.Sub(origin).Seconds()
			}
			words := make([]string, 0, len(entry.words))
			for _, word := range entry.words {
				wordStart, hasWordStart := subtitleWordOffset(word.start, origin, relative)
				wordEnd, hasWordEnd := subtitleWordOffset(word.end, origin, relative)
				if hasWordStart && (wordStart < start || wordStart > end) || hasWordEnd && (wordEnd < start || wordEnd > end) || hasWordStart && hasWordEnd && wordEnd < wordStart {
					return nil, errors.New("transcript word timing is outside cue bounds")
				}
				words = append(words, word.text)
			}
			cues = append(cues, subtitleSegment{entry.speaker, strings.Join(words, " "), start, end})
		}
	default:
		return nil, fmt.Errorf("recording start is unknown or timing is unavailable for %s", format)
	}
	if len(cues) == 0 {
		return nil, errors.New("a complete timed transcript is unavailable")
	}
	return cues, nil
}

func subtitleWordOffset(stamp map[string]jsontext.Value, origin time.Time, relative bool) (float64, bool) {
	if relative {
		return rawFiniteFloat(stamp["relative"])
	}
	at, ok := rawTime(stamp["absolute"], false)
	if !ok {
		return 0, false
	}
	return at.Sub(origin).Seconds(), true
}
