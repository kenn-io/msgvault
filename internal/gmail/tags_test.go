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
	change := emailtags.MessageTagChange{Add: []string{"Label_new"}, Remove: []string{"Label_old"}, DryRun: true}
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
	for _, id := range []string{"INBOX", "New"} {
		_, err = c.MessageTags(t.Context(), "message-1", &emailtags.MessageTagChange{Add: []string{id}})
		require.Error(err)
	}
	assert.Equal(1, writes)
}

func TestMessageTagsDistinguishesUnreadAndEmptyState(t *testing.T) {
	for _, scenario := range []string{"unavailable tag", "metadata failure", "observed empty"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/labels") {
					_, _ = w.Write([]byte(`{"labels":[]}`))
					return
				}
				if scenario == "metadata failure" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(`{"id":"message-1"}`))
			}))
			var change *emailtags.MessageTagChange
			if scenario == "unavailable tag" {
				change = &emailtags.MessageTagChange{Add: []string{"Label_missing"}}
			}
			result, err := c.MessageTags(t.Context(), "message-1", change)
			if scenario == "observed empty" {
				require.NoError(err)
				require.NotNil(result)
				assert.Equal([]string{}, result.Tags)
				assert.Equal([]string{}, result.Before)
				assert.True(result.Verified)
				return
			}
			var failure *emailtags.MessageTagError
			require.ErrorAs(err, &failure)
			assert.Nil(result)
			assert.Nil(failure.Result)
		})
	}
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
	result, err := c.MessageTags(t.Context(), "message-1", &emailtags.MessageTagChange{Add: []string{"Label_new"}})
	require.Error(err)
	assert.False(result.Verified)
	var failure *emailtags.MessageTagError
	require.ErrorAs(err, &failure)
	assert.Equal("verification_failed", failure.Code)
	assert.Equal(result, failure.Result)
}

func TestMessageTagsReadbackFailurePreservesObservation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
		clear      bool
	}{
		{"matching", `{"id":"message-1","labelIds":["INBOX","Label_new"]}`, []string{"INBOX", "Label_new"}, false},
		{"different message", `{"id":"other-message","labelIds":["Label_new"]}`, []string{"INBOX"}, false},
		{"malformed", `{`, []string{"INBOX"}, false},
		{"empty tags omitted", `{"id":"message-1"}`, []string{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			reads := 0
			before := []string{"INBOX"}
			change := emailtags.MessageTagChange{Add: []string{"Label_new"}}
			if tc.clear {
				before = []string{"Label_new"}
				change = emailtags.MessageTagChange{Remove: []string{"Label_new"}}
			}
			c := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/labels"):
					_, _ = w.Write([]byte(`{"labels":[{"id":"Label_new","name":"New","type":"user"}]}`))
				case strings.HasSuffix(r.URL.Path, "/modify"):
					_, _ = w.Write([]byte(tc.body))
				default:
					reads++
					if reads > 1 {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"id": "message-1", "labelIds": before}))
				}
			}))
			result, err := c.MessageTags(t.Context(), "message-1", &change)
			var failure *emailtags.MessageTagError
			require.ErrorAs(err, &failure)
			assert.Equal("remote_unknown", failure.Code)
			require.NotNil(result)
			assert.Equal(before, result.Before)
			assert.Equal(tc.want, result.Tags)
			assert.False(result.Verified)
			assert.Equal(result, failure.Result)
			assert.Equal(2, reads)
		})
	}
}
