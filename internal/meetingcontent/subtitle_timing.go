package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"
)

func subtitleTiming(format string, raw []byte) ([]subtitleSegment, error) {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errors.New("invalid raw transcript")
	}
	var cues []subtitleSegment
	switch format {
	case "omi_json":
		origin, ok := rawTime(fields["started_at"], false)
		if !ok || origin.IsZero() {
			return nil, errors.New("recording start is unknown")
		}
		transcript, _ := decodeOmiTranscript(fields)
		if transcript.State != StateAvailable {
			return nil, errors.New("invalid transcript")
		}
		var items []struct {
			Text *string  `json:"text"`
			End  *float64 `json:"end"`
		}
		_ = json.Unmarshal(fields["transcript_segments"], &items)
		cursor := 0
		for _, item := range items {
			if strings.TrimSpace(*item.Text) == "" {
				continue
			}
			seg := transcript.Segments[cursor]
			cursor++
			if seg.OffsetSeconds == nil || item.End == nil {
				return nil, errors.New("transcript needs start and end times")
			}
			cues = append(cues, subtitleSegment{seg.Speaker, *item.Text, *seg.OffsetSeconds, *item.End})
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
		if transcript.State != StateAvailable || transcript.Reason != "" {
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
			if item.Start == nil || item.End == nil || seg.OffsetSeconds == nil {
				return nil, errors.New("retained transcript timing does not match archived transcript")
			}
			cues = append(cues, subtitleSegment{seg.Speaker, seg.Text, *item.Start, *item.End})
		}

	case "twenty_json":
		var recording map[string]jsontext.Value
		_ = json.Unmarshal(fields["recording"], &recording)
		transcript := decodeTwenty(fields).Transcript
		entries, _ := twentyEntries(recording["transcript"])
		if transcript.State != StateAvailable {
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
			relative := hasStart && hasEnd
			if !hasStart || !hasEnd {
				startTime, startOK := rawTime(startStamp["absolute"], false)
				endTime, endOK := rawTime(endStamp["absolute"], false)
				if !startOK || !endOK {
					return nil, errors.New("transcript needs matching start and end times")
				}
				start, end = startTime.Sub(origin).Seconds(), endTime.Sub(origin).Seconds()
			}
			for _, word := range entry.words {
				wordStart, hasWordStart := subtitleWordOffset(word.start, origin, relative)
				wordEnd, hasWordEnd := subtitleWordOffset(word.end, origin, relative)
				if hasWordStart && (wordStart < start || wordStart > end) || hasWordEnd && (wordEnd < start || wordEnd > end) || hasWordStart && hasWordEnd && wordEnd < wordStart {
					return nil, errors.New("transcript word timing is outside cue bounds")
				}
			}
			seg := transcript.Segments[i]
			cues = append(cues, subtitleSegment{seg.Speaker, seg.Text, start, end})
		}
	default:
		return nil, errors.New("recording start or timing is unavailable for this source")
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
