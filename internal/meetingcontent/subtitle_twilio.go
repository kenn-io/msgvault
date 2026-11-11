package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/twilioevidence"
)

func twilioSubtitleTiming(fields map[string]jsontext.Value) ([]subtitleSegment, error) {
	var recordings []struct {
		SID       string `json:"sid"`
		StartTime string `json:"start_time"`
	}
	var evidence twilioevidence.Evidence
	var archived []twilioevidence.Segment
	if json.Unmarshal(fields["recordings"], &recordings) != nil || json.Unmarshal(fields["evidence"], &evidence) != nil || json.Unmarshal(fields["transcript_segments"], &archived) != nil {
		return nil, errors.New("retained transcript timing is unavailable")
	}
	selected, _ := twilioevidence.SelectTranscripts(evidence.Transcripts)
	if len(selected) != 1 {
		return nil, errors.New("subtitle export needs one recording timeline")
	}
	for key, tr := range selected {
		if tr.Kind != "classic" || tr.SourceID != key || len(tr.Segments) != len(archived) {
			return nil, errors.New("retained transcript timing is unavailable")
		}
		var matches int
		var origin bool
		for _, recording := range recordings {
			if recording.SID == key {
				matches++
				origin = !twilioevidence.ParseTime(recording.StartTime).IsZero()
			}
		}
		if matches != 1 || !origin {
			return nil, errors.New("recording start is unknown")
		}
		var raw struct {
			Sentences []jsontext.Value `json:"sentences"`
		}
		if json.Unmarshal(tr.Raw, &raw) != nil {
			return nil, errors.New("retained transcript timing is unavailable")
		}
		sentences, err := twilioevidence.DecodeSentences(raw.Sentences)
		if err != nil {
			return nil, errors.New("invalid transcript sentence")
		}
		cues := make([]subtitleSegment, 0, len(archived))
		for _, sentence := range sentences {
			if sentence.Text == nil || strings.TrimSpace(*sentence.Text) == "" {
				continue
			}
			i := len(cues)
			if i >= len(archived) || sentence.Start == nil || sentence.End == nil || archived[i].OffsetSeconds == nil || tr.Segments[i].OffsetSeconds == nil || *sentence.Start != *archived[i].OffsetSeconds || *sentence.Start != *tr.Segments[i].OffsetSeconds || *sentence.Text != tr.Segments[i].Text || archived[i].Text != tr.Segments[i].Text || archived[i].Speaker != tr.Segments[i].Speaker || archived[i].Scope != key || tr.Segments[i].Scope != key {
				return nil, errors.New("retained transcript timing does not match archived transcript")
			}
			cues = append(cues, subtitleSegment{archived[i].Speaker, archived[i].Text, *sentence.Start, *sentence.End})
		}
		if len(cues) != len(archived) || len(cues) == 0 {
			return nil, errors.New("retained transcript timing is incomplete")
		}
		return cues, nil
	}
	return nil, errors.New("retained transcript timing is unavailable")
}
