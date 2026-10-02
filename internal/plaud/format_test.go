package plaud

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteIDChangesReplaceEditedBody(t *testing.T) {
	for _, ids := range [][2]string{{"stable", ""}, {"", "stable"}, {"old-id", "new-id"}} {
		t.Run(ids[0]+"-to-"+ids[1], func(t *testing.T) {
			old := evidence{Notes: []Note{{ID: ids[0], Type: "auto_sum_note", Content: "Old summary"}}}
			next := evidence{Notes: []Note{{ID: ids[1], Type: "auto_sum_note", Content: "Edited summary"}}}
			next.preserve(old)
			require.Len(t, next.Notes, 1)
			assert.Equal(t, "Edited summary", next.Notes[0].Content)
			if ids[1] != "" {
				assert.Equal(t, ids[1], next.Notes[0].ID)
			}
		})
	}
}

func TestAmbiguousNoteTypesCannotOverwriteMissingTabs(t *testing.T) {
	old := evidence{Notes: []Note{{ID: "one", Type: "auto_sum_note", Content: "First"}, {ID: "two", Type: "auto_sum_note", Content: "Second"}}}
	next := evidence{Notes: []Note{{Type: "auto_sum_note", Content: "Unknown tab"}}}
	next.preserve(old)
	assert.Len(t, next.Notes, 3)
	again := evidence{Notes: []Note{{Type: "auto_sum_note", Content: "Unknown tab"}}}
	again.preserve(next)
	assert.Equal(t, next.Notes, again.Notes, "repeating the same partial response must not accumulate tabs")
}
