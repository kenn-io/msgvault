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
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "Label_keep", "name": "Keep", "type": "user"}}}))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/modify") {
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&delta))
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
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": "message-1", "labelIds": tags}))
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

func TestArchiveMessageReadbackFailures(t *testing.T) {
	for _, scenario := range []string{"read failure", "ignored write"} {
		t.Run(scenario, func(t *testing.T) {
			reads := 0
			c := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/labels") {
					_, _ = w.Write([]byte(`{"labels":[]}`))
					return
				}
				if r.Method == http.MethodGet {
					reads++
					if reads > 1 && scenario == "read failure" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
				}
				_, _ = w.Write([]byte(`{"id":"message-1","labelIds":["INBOX","UNREAD"]}`))
			}))
			result, err := c.ArchiveMessage(t.Context(), "message-1", true, false)
			require.Error(t, err)
			require.NotNil(t, result)
			assert.False(t, result.Verified)
			assert.Equal(t, 2, reads)
			assert.Contains(t, result.Tags, "INBOX")
		})
	}
}
