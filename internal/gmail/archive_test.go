package gmail

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveMessageDeltaPreviewAndRetry(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
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
	requirementsForTest.NoError(err)
	assertionsForTest.True(preview.DryRun)
	assertionsForTest.NotContains(preview.Tags, "INBOX")
	assertionsForTest.Zero(writes)
	for _, archived := range []bool{true, true, false, false} {
		result, err := c.ArchiveMessage(t.Context(), "message-1", archived, false)
		requirementsForTest.NoError(err)
		assertionsForTest.True(result.Verified)
		assertionsForTest.Equal(!archived, slices.Contains(result.Tags, "INBOX"))
		assertionsForTest.Contains(result.Tags, "UNREAD")
		assertionsForTest.Contains(result.Tags, "Label_keep")
	}
	assertionsForTest.Equal(2, writes)
}
