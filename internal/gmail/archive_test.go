package gmail

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveMessageDeltaPreviewAndRetry(t *testing.T) {
	writes := 0
	c := newTagDeltaTestClient(t, []string{"INBOX", "UNREAD", "Label_keep"}, func(add, remove, tags []string) {
		if slices.Contains(tags, "INBOX") {
			assert.Equal(t, []string{"INBOX"}, remove)
			assert.Empty(t, add)
		} else {
			assert.Equal(t, []string{"INBOX"}, add)
			assert.Empty(t, remove)
		}
		writes++
	})
	preview, err := c.ArchiveMessage(t.Context(), "message-1", true, true)
	require.NoError(t, err)
	assert.True(t, preview.DryRun)
	assert.NotContains(t, preview.Tags, "INBOX")
	assert.Zero(t, writes)
	for _, archived := range []bool{true, true, false, false} {
		result, err := c.ArchiveMessage(t.Context(), "message-1", archived, false)
		require.NoError(t, err)
		assert.True(t, result.Verified)
		assert.Equal(t, !archived, slices.Contains(result.Tags, "INBOX"))
		assert.Contains(t, result.Tags, "UNREAD")
		assert.Contains(t, result.Tags, "Label_keep")
	}
	assert.Equal(t, 2, writes)
}
