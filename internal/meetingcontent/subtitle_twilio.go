package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type twilioSubtitleSegment struct {
	Speaker string   `json:"speaker"`
	Text    string   `json:"text"`
	Scope   string   `json:"scope"`
	Start   *float64 `json:"offset_seconds"`
}
type twilioSubtitleTranscript struct {
	Kind     string                  `json:"kind"`
	ID       string                  `json:"id"`
	SourceID string                  `json:"source_id"`
	Created  string                  `json:"date_created"`
	Usable   bool                    `json:"usable"`
	Segments []twilioSubtitleSegment `json:"segments"`
	Raw      jsontext.Value          `json:"raw"`
}

func twilioSubtitleTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, "2006-01-02T15:04:05Z0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func twilioSubtitleTiming(fields map[string]jsontext.Value, archived []Segment) ([]subtitleSegment, error) {
	var evidence struct {
		Transcripts []twilioSubtitleTranscript `json:"transcripts"`
	}
	var recordings []struct {
		SID   string `json:"sid"`
		Start string `json:"start_time"`
	}
	if json.Unmarshal(fields["evidence"], &evidence) != nil || json.Unmarshal(fields["recordings"], &recordings) != nil {
		return nil, errors.New("recording timing unavailable")
	}
	starts := make(map[string]time.Time, len(recordings))
	for _, rec := range recordings {
		starts[rec.SID] = twilioSubtitleTime(rec.Start)
	}
	selected := map[string]twilioSubtitleTranscript{}
	for _, tr := range evidence.Transcripts {
		if !tr.Usable {
			continue
		}
		key := tr.SourceID
		if key == "" {
			key = tr.ID
		}
		old, found := selected[key]
		newer := tr.Kind == "classic" && old.Kind != "classic"
		if (tr.Kind == "classic") == (old.Kind == "classic") {
			a, b := twilioSubtitleTime(tr.Created), twilioSubtitleTime(old.Created)
			newer = a.After(b) || a.Equal(b) && tr.ID > old.ID
		}
		if !found || newer {
			selected[key] = tr
		}
	}
	keys := make([]string, 0, len(selected))
	var origin time.Time
	for key := range selected {
		start, found := starts[key]
		if !found || start.IsZero() {
			return nil, errors.New("recording start is unknown")
		}
		if !origin.IsZero() && !origin.Equal(start) {
			return nil, errors.New("multiple recording origins cannot be aligned")
		}
		origin = start
		keys = append(keys, key)
	}
	sort.Strings(keys)
	cues := make([]subtitleSegment, 0, len(archived))
	for n, key := range keys {
		tr := selected[key]
		var raw struct {
			Sentences []jsontext.Value `json:"sentences"`
		}
		if tr.Kind != "classic" || json.Unmarshal(tr.Raw, &raw) != nil || raw.Sentences == nil {
			return nil, errors.New("retained sentence timing unavailable")
		}
		type sentence struct {
			SID     string         `json:"sid"`
			Index   int            `json:"sentence_index"`
			Channel jsontext.Value `json:"media_channel"`
			Start   jsontext.Value `json:"start_time"`
			End     jsontext.Value `json:"end_time"`
			Text    *string        `json:"transcript"`
		}
		sentences := make([]sentence, 0, len(raw.Sentences))
		for _, value := range raw.Sentences {
			var item sentence
			if json.Unmarshal(value, &item) != nil {
				return nil, errors.New("invalid sentence")
			}
			sentences = append(sentences, item)
		}
		sort.SliceStable(sentences, func(i, j int) bool { return sentences[i].Index < sentences[j].Index })
		seen := map[string]bool{}
		index := 0
		for _, item := range sentences {
			identity := item.SID
			if identity == "" {
				channel, _ := rawFiniteFloat(item.Channel)
				identity = fmt.Sprintf("%d:%d", item.Index, int(channel))
			}
			if seen[identity] {
				continue
			}
			seen[identity] = true
			if item.Text == nil || strings.TrimSpace(*item.Text) == "" {
				continue
			}
			if index >= len(tr.Segments) || len(cues) >= len(archived) {
				return nil, errors.New("retained transcript timing does not match archived transcript")
			}
			seg := tr.Segments[index]
			index++
			start, okStart := rawFiniteFloat(item.Start)
			end, okEnd := rawFiniteFloat(item.End)
			if !okStart || !okEnd || seg.Start == nil || *seg.Start != start || seg.Text != *item.Text || seg.Scope != key {
				return nil, errors.New("retained transcript timing does not match archived transcript")
			}
			speaker := seg.Speaker
			if len(keys) > 1 && seg.Scope != "" {
				speaker = fmt.Sprintf("Recording %d", n+1)
				if seg.Speaker != "" {
					speaker += " / " + seg.Speaker
				}
			}
			target := archived[len(cues)]
			if strings.TrimSpace(seg.Text) != target.Text || strings.TrimSpace(speaker) != target.Speaker || len(keys) == 1 && (target.OffsetSeconds == nil || *target.OffsetSeconds != start) {
				return nil, errors.New("retained transcript timing does not match archived transcript")
			}
			cues = append(cues, subtitleSegment{target.Speaker, target.Text, start, end})
		}
		if index != len(tr.Segments) {
			return nil, errors.New("retained transcript timing is incomplete")
		}
	}
	if len(cues) != len(archived) {
		return nil, errors.New("retained transcript timing is incomplete")
	}
	return cues, nil
}
