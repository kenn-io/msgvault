package plaud

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func decodeFile(raw jsontext.Value) (File, error) {
	var v struct {
		ID       string         `json:"id"`
		Name     string         `json:"name"`
		Start    jsontext.Value `json:"start_at"`
		Created  jsontext.Value `json:"created_at"`
		Updated  jsontext.Value `json:"updated_at"`
		Duration float64        `json:"duration"`
		Sources  []struct {
			Type string `json:"data_type"`
		} `json:"source_list"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || strings.TrimSpace(v.ID) == "" || v.Duration < 0 || math.IsNaN(v.Duration) || math.IsInf(v.Duration, 0) {
		return File{}, fmt.Errorf("%w: invalid file metadata", ErrContract)
	}
	f := File{ID: v.ID, Name: v.Name, DurationMS: v.Duration}
	var err error
	if f.StartedAt, err = decodeTime(v.Start); err != nil {
		return File{}, err
	}
	if f.CreatedAt, err = decodeTime(v.Created); err != nil {
		return File{}, err
	}
	if f.UpdatedAt, err = decodeTime(v.Updated); err != nil {
		return File{}, err
	}
	for _, s := range v.Sources {
		if s.Type == "" {
			return File{}, fmt.Errorf("%w: untyped source block", ErrContract)
		}
		f.Blocks = append(f.Blocks, s.Type)
	}
	return f, nil
}

// Plaud timestamps may be ISO strings or Unix seconds/milliseconds.
func decodeTime(raw jsontext.Value) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return time.Time{}, nil
	}
	text := string(raw)
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(raw, &text); err != nil {
			return time.Time{}, fmt.Errorf("%w: invalid timestamp", ErrContract)
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return t.UTC(), nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", time.DateOnly} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), nil
		}
	}
	if n, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 && n < 1e15 {
		if n >= 1e12 {
			n /= 1000
		}
		sec, frac := math.Modf(n)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%w: unrecognized timestamp", ErrContract)
}

func decodeNotes(raw jsontext.Value) ([]Note, error) {
	var items []struct {
		ID      string `json:"id"`
		Type    string `json:"data_type"`
		Content string `json:"data_content"`
		Failure string `json:"data_content_error"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, fmt.Errorf("%w: invalid notes inventory", ErrContract)
	}
	out := make([]Note, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		if item.Failure != "" {
			return nil, fmt.Errorf("%w: note content fetch failed", ErrContract)
		}
		if item.Type == "" {
			return nil, fmt.Errorf("%w: untyped note", ErrContract)
		}
		if item.ID != "" && seen[item.ID] {
			return nil, fmt.Errorf("%w: duplicate note identity", ErrContract)
		}
		seen[item.ID] = true
		out = append(out, Note{ID: item.ID, Type: item.Type, Content: item.Content})
	}
	return out, nil
}

func decodeSegment(raw jsontext.Value) (Segment, error) {
	var v struct {
		Speaker jsontext.Value `json:"speaker"`
		Content *string        `json:"content"`
		Start   *float64       `json:"start_time"`
		End     *float64       `json:"end_time"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Content == nil || strings.TrimSpace(*v.Content) == "" {
		return Segment{}, fmt.Errorf("%w: invalid transcript segment", ErrContract)
	}
	s := Segment{Text: *v.Content, Speaker: "Unknown speaker"}
	if v.Start != nil {
		if *v.Start < 0 || math.IsNaN(*v.Start) || math.IsInf(*v.Start, 0) {
			return Segment{}, fmt.Errorf("%w: invalid transcript segment", ErrContract)
		}
		s.StartSeconds = *v.Start
	}
	if v.End != nil {
		if *v.End < s.StartSeconds || math.IsNaN(*v.End) || math.IsInf(*v.End, 0) {
			return Segment{}, fmt.Errorf("%w: invalid transcript end time", ErrContract)
		}
		s.EndSeconds = *v.End
	}
	if len(v.Speaker) > 0 && string(v.Speaker) != "null" {
		var name string
		if err := json.Unmarshal(v.Speaker, &name); err == nil {
			if strings.TrimSpace(name) != "" {
				s.Speaker = name
			}
		} else {
			var n int
			if err := json.Unmarshal(v.Speaker, &n); err != nil || n < 0 {
				return Segment{}, fmt.Errorf("%w: invalid speaker label", ErrContract)
			}
			s.Speaker = fmt.Sprintf("Speaker %d", n)
		}
	}
	return s, nil
}
