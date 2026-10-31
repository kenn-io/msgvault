package meetingcontent

import (
	"encoding/json/v2"
	"testing"

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
	for _, mutate := range []func(*Content){func(c *Content) { c.Summary.State = "unexpected" }, func(c *Content) { c.ActionCoverage = "unexpected" }, func(c *Content) { c.Actions = []Action{{Title: "Task", Status: "unexpected"}} }, func(c *Content) { v := -1.0; c.DurationSeconds = &v; c.DurationBasis = DurationProvider }} {
		invalid := content
		mutate(&invalid)
		raw, err = json.Marshal(map[string]any{"schema_version": 1, "content": invalid})
		requirements.NoError(err)
		assertions.Equal(StateUnavailable, Decode("pocket_json", raw, nil).Summary.State)
	}
}
