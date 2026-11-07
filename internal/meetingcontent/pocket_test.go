package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodePocketEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := Content{Summary: Section{State: StateAvailable, Text: "Decision"}, Notes: Section{State: StateUnsupported}, Transcript: Transcript{State: StateEmpty}, Actions: []Action{}, ActionCoverage: CoverageAvailable}
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "content": content})
	requirements.NoError(err)
	assertions.Equal(content, Decode("pocket_json", raw, nil))
	raw, err = json.Marshal(map[string]any{"schema_version": 2, "content": content})
	requirements.NoError(err)
	assertions.Equal(content, Decode("pocket_json", raw, nil))
	raw, err = json.Marshal(map[string]any{"schema_version": 3, "content": content})
	requirements.NoError(err)
	assertions.Equal(StateUnavailable, Decode("pocket_json", raw, nil).Summary.State)
	for _, mutate := range []func(*Content){func(c *Content) { c.Summary.State = "unexpected" }, func(c *Content) { c.ActionCoverage = "unexpected" }, func(c *Content) { c.Actions = []Action{{Title: "Task", Status: "unexpected"}} }, func(c *Content) { v := -1.0; c.DurationSeconds = &v; c.DurationBasis = DurationProvider }} {
		invalid := content
		mutate(&invalid)
		raw, err = json.Marshal(map[string]any{"schema_version": 1, "content": invalid})
		requirements.NoError(err)
		assertions.Equal(StateUnavailable, Decode("pocket_json", raw, nil).Summary.State)
	}
	first := `{"speakerName":"A","text":"First","start":0,"end":1}`
	second := `{"speakerName":"B","text":"Second","start":1,"end":2}`
	matching := `{"transcript":{"segments":[` + first + `,` + second + `]}}`
	for _, test := range []struct {
		name, recording string
		mutate          func(*Transcript)
		end             *float64
	}{
		{name: "matching legacy", recording: matching, end: new(1.0)},
		{name: "zero end", recording: strings.Replace(matching, `"end":1`, `"end":0`, 1), end: new(0.0)},
		{name: "missing end", recording: strings.Replace(matching, `,"end":1`, "", 1)},
		{name: "missing start", recording: strings.Replace(matching, `"start":0,`, "", 1)},
		{name: "text", recording: strings.Replace(matching, "First", "Changed", 1)},
		{name: "speaker", recording: strings.Replace(matching, `"A"`, `"Other"`, 1)},
		{name: "start", recording: strings.Replace(matching, `"start":0`, `"start":0.5`, 1)},
		{name: "order", recording: `{"transcript":{"segments":[` + second + `,` + first + `]}}`},
		{name: "count", recording: `{"transcript":{"segments":[` + first + `]}}`},
		{name: "known end", recording: strings.Replace(matching, `"end":2`, `"end":3`, 1)},
		{name: "pending", recording: strings.Replace(matching, `{"transcript":`, `{"transcript_error":"processing failed","transcript":`, 1)},
		{name: "invalid", recording: `{"transcript":{"segments":[{"text":"First","start":-1}]}}`},
		{name: "state", recording: matching, mutate: func(tr *Transcript) { tr.State = StateUnavailable }},
		{name: "reason", recording: matching, mutate: func(tr *Transcript) { tr.Reason = "retained reason" }},
		{name: "full text", recording: matching, mutate: func(tr *Transcript) { tr.Text = "retained full text" }},
		{name: "start presence", recording: matching, mutate: func(tr *Transcript) { tr.Segments[0].OffsetSeconds = nil }},
		{name: "other coordinates", recording: matching, mutate: func(tr *Transcript) { tr.Segments[0].StartedAt = new(time.Unix(0, 0).UTC()) }},
	} {
		canonical := content
		canonical.Transcript = Transcript{State: StateAvailable, Segments: []Segment{{Speaker: "A", Text: "First", OffsetSeconds: new(0.0)}, {Speaker: "B", Text: "Second", OffsetSeconds: new(1.0), EndOffsetSeconds: new(2.0)}}}
		if test.mutate != nil {
			test.mutate(&canonical.Transcript)
		}
		raw, err := json.Marshal(map[string]any{"schema_version": 1, "content": canonical, "recording": jsontext.Value(test.recording)})
		requirements.NoError(err)
		before := append([]byte(nil), raw...)
		expected := canonical
		expected.Transcript.Segments = append([]Segment(nil), canonical.Transcript.Segments...)
		if test.end != nil {
			expected.Transcript.Segments[0].EndOffsetSeconds = test.end
		}
		assertions.Equal(expected, Decode("pocket_json", raw, nil), test.name)
		assertions.Equal(before, raw, test.name)
	}
}
