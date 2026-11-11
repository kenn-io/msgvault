// Package twilioevidence owns retained Twilio transcript evidence.
package twilioevidence

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

type Segment struct {
	Speaker       string   `json:"speaker,omitempty"`
	Text          string   `json:"text"`
	Scope         string   `json:"scope,omitempty"`
	OffsetSeconds *float64 `json:"offset_seconds,omitempty"`
}

type Transcript struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	SourceID string `json:"source_id"`
	Status   string `json:"status"`
	// DateCreated tells a retranscription from the transcript it replaced.
	DateCreated string         `json:"date_created,omitempty"`
	Complete    bool           `json:"complete"`
	Usable      bool           `json:"usable"`
	Segments    []Segment      `json:"segments,omitempty"`
	Raw         jsontext.Value `json:"raw,omitempty"`
}

type Evidence struct {
	Transcripts []Transcript `json:"transcripts,omitempty"`
	Diagnostics []string     `json:"diagnostics,omitempty"`
}

// ParseTime accepts the timestamp formats returned by Voice and Intelligence.
func ParseTime(value string) time.Time {
	for _, format := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, "2006-01-02T15:04:05Z0700"} {
		if parsed, err := time.Parse(format, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

type Sentence struct {
	SID     string   `json:"sid"`
	Index   int      `json:"sentence_index"`
	Channel *int     `json:"media_channel"`
	Start   *float64 `json:"start_time"`
	End     *float64 `json:"end_time"`
	Text    *string  `json:"transcript"`
}

func (s *Sentence) UnmarshalJSON(data []byte) error {
	var value struct {
		SID     string         `json:"sid"`
		Index   int            `json:"sentence_index"`
		Channel jsontext.Value `json:"media_channel"`
		Start   jsontext.Value `json:"start_time"`
		End     jsontext.Value `json:"end_time"`
		Text    *string        `json:"transcript"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parse := func(raw jsontext.Value) (float64, bool, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return 0, false, nil
		}
		text := string(raw)
		if raw[0] == '"' {
			if err := json.Unmarshal(raw, &text); err != nil {
				return 0, false, err
			}
		}
		v, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0, false, errors.New("invalid sentence offset")
		}
		return v, true, nil
	}
	startValue, hasStart, err := parse(value.Start)
	if err != nil {
		return err
	}
	endValue, hasEnd, err := parse(value.End)
	if err != nil {
		return err
	}
	channelValue, hasChannel, err := parse(value.Channel)
	if err != nil {
		return err
	}
	var channel *int
	if hasChannel {
		if math.Trunc(channelValue) != channelValue || channelValue > 2 {
			return errors.New("invalid media channel")
		}
		v := int(channelValue)
		channel = &v
	}
	var start, end *float64
	if hasStart {
		start = &startValue
	}
	if hasEnd {
		end = &endValue
	}
	*s = Sentence{SID: value.SID, Index: value.Index, Channel: channel, Start: start, End: end, Text: value.Text}
	return nil
}

// SelectTranscripts applies the producer's per-recording transcript preference.
func SelectTranscripts(transcripts []Transcript) (map[string]Transcript, bool) {
	selected := map[string]Transcript{}
	complete := false
	for _, tr := range transcripts {
		complete = complete || tr.Complete
		if !tr.Usable {
			continue
		}
		key := tr.SourceID
		if key == "" {
			key = tr.ID
		}
		prior, ok := selected[key]
		if !ok || supersedes(tr, prior) {
			selected[key] = tr
		}
	}
	return selected, complete
}
func supersedes(tr, prior Transcript) bool {
	if classic, priorClassic := tr.Kind == "classic", prior.Kind == "classic"; classic != priorClassic {
		return classic
	}
	created, priorCreated := ParseTime(tr.DateCreated), ParseTime(prior.DateCreated)
	if !created.Equal(priorCreated) {
		return created.After(priorCreated)
	}
	return tr.ID > prior.ID
}

// DecodeSentences applies the producer's sentence ordering and deduplication.
func DecodeSentences(raw []jsontext.Value) ([]Sentence, error) {
	parsed := make([]Sentence, 0, len(raw))
	for _, value := range raw {
		var item Sentence
		if err := json.Unmarshal(value, &item); err != nil {
			return nil, err
		}
		parsed = append(parsed, item)
	}
	sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].Index < parsed[j].Index })
	seen := map[string]bool{}
	out := parsed[:0]
	for _, item := range parsed {
		channel := 0
		if item.Channel != nil {
			channel = *item.Channel
		}
		key := fmt.Sprintf("%d:%d", item.Index, channel)
		if item.SID != "" {
			key = item.SID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out, nil
}
