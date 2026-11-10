package gmail

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveMessageDeltaPreviewAndRetry(t *testing.T) {
	tags := []string{"INBOX", "UNREAD", "Label_keep"}
	writes := 0
	c := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/labels") {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}, {"id": "Label_keep", "name": "Keep", "type": "user"}}}))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/modify") {
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&delta)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if slices.Contains(tags, "INBOX") {
				assert.Equal(t, []string{"INBOX"}, delta.Remove)
				assert.Empty(t, delta.Add)
			} else {
				assert.Equal(t, []string{"INBOX"}, delta.Add)
				assert.Empty(t, delta.Remove)
			}
			writes++
			tags = slices.DeleteFunc(tags, func(s string) bool { return slices.Contains(delta.Remove, s) })
			tags = append(tags, delta.Add...)
		} else {
			assert.Equal(t, "/gmail/v1/users/me/messages/message-1", r.URL.Path)
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": "message-1", "labelIds": tags}))
	}))
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
