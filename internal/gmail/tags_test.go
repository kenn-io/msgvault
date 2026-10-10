package gmail

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestMessageTagsNativeDeltaPreviewAndRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tags := []string{"INBOX", "UNREAD", "Label_old", "Label_other"}
	writes := 0
	c := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "Inbox", "type": "system"}, {"id": "Label_old", "name": "Old", "type": "user"}, {"id": "Label_new", "name": "New", "type": "user"}, {"id": "Label_other", "name": "Other", "type": "user"}}})
		case "/gmail/v1/users/me/messages/message-1":
			assert.Equal("metadata", r.URL.Query().Get("format"))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "message-1", "labelIds": tags})
		case "/gmail/v1/users/me/messages/message-1/modify":
			assert.Equal(http.MethodPost, r.Method)
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&delta)) {
				return
			}
			assert.Equal([]string{"Label_new"}, delta.Add)
			assert.Equal([]string{"Label_old"}, delta.Remove)
			writes++
			tags = slices.DeleteFunc(tags, func(s string) bool { return slices.Contains(delta.Remove, s) })
			tags = append(tags, delta.Add...)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "message-1", "labelIds": tags})
		default:
			http.NotFound(w, r)
		}
	}))
	change := emailtags.Change{Add: []string{"Label_new"}, Remove: []string{"Label_old"}, DryRun: true}
	preview, err := c.MessageTags(t.Context(), "message-1", &change)
	require.NoError(err)
	assert.Zero(writes)
	assert.True(preview.DryRun)
	assert.False(preview.Verified)
	assert.Contains(preview.Tags, "Label_new")
	change.DryRun = false
	got, err := c.MessageTags(t.Context(), "message-1", &change)
	require.NoError(err)
	assert.True(got.Verified)
	assert.ElementsMatch([]string{"INBOX", "UNREAD", "Label_new", "Label_other"}, got.Tags)
	require.Len(got.AvailableTags, 3)
	_, err = c.MessageTags(t.Context(), "message-1", &change)
	require.NoError(err)
	assert.Equal(1, writes, "satisfied retry must not write")
	for _, id := range []string{"INBOX", "UNREAD", "New", "Label_missing"} {
		_, err = c.MessageTags(t.Context(), "message-1", &emailtags.Change{Add: []string{id}})
		require.Error(err)
	}
	assert.Equal(1, writes)
}

func TestMessageTagsIgnoredWriteHasResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[{"id":"Label_new","name":"New","type":"user"}]}`))
		default:
			_, _ = w.Write([]byte(`{"id":"message-1","labelIds":["INBOX"]}`))
		}
	}))
	result, err := c.MessageTags(t.Context(), "message-1", &emailtags.Change{Add: []string{"Label_new"}})
	require.Error(err)
	assert.False(result.Verified)
	var failure *emailtags.Error
	require.ErrorAs(err, &failure)
	assert.Equal("verification_failed", failure.Code)
	assert.Equal(result, failure.Result)
}

func TestMessageTagsModifyIsUncertainMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	assert.True(OpMessagesModify.remoteMutation())
	assert.Equal(5, OpMessagesModify.Cost())
	requests := 0
	c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		return deadlineResponse(http.StatusInternalServerError, `{"error":{"code":500}}`), nil
	})
	_, err := c.request(t.Context(), OpMessagesModify, http.MethodPost, "/users/me/messages/message-1/modify", []byte(`{"addLabelIds":["Label_new"]}`))
	require.Error(err)
	assert.Equal(1, requests, "an uncertain write must not retry automatically")
}

func TestMessageTagsReadbackFailurePreservesObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	reads := 0
	c := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`{"labels":[{"id":"Label_new","name":"New","type":"user"}]}`))
		case strings.HasSuffix(r.URL.Path, "/modify"):
			_, _ = w.Write([]byte(`{"id":"message-1","labelIds":["INBOX","Label_new"]}`))
		default:
			reads++
			if reads > 1 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":404}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"message-1","labelIds":["INBOX"]}`))
		}
	}))
	result, err := c.MessageTags(t.Context(), "message-1", &emailtags.Change{Add: []string{"Label_new"}})
	require.Error(err)
	require.NotNil(result)
	var failure *emailtags.Error
	require.ErrorAs(err, &failure)
	assert.Equal("remote_unknown", failure.Code)
	assert.Equal([]string{"INBOX"}, result.Tags)
	assert.False(result.Verified)
	assert.Equal(result, failure.Result)
}
