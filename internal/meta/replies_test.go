package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplyToCommentExactRequest(t *testing.T) {
	calls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/v26.0/100_200_300/comments", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "Example reply", r.PostForm.Get("message"))
		assert.Equal(t, "id,message,created_time,from,parent", r.PostForm.Get("fields"))
		_, _ = fmt.Fprint(w, `{"id":"100_200_301","message":"Example reply","created_time":"2026-11-01T12:00:00+0000","from":{"id":"100"},"parent":{"id":"100_200_300"}}`)
	})
	got, err := c.ReplyToComment(t.Context(), "100_200_300", "Example reply")
	require.NoError(t, err)
	assert.Equal(t, "100_200_301", got.ID)
	assert.Equal(t, 1, calls)
}
func TestPrivateReplyExactJSON(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/v26.0/100/messages", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		var got map[string]any
		require.NoError(t, json.UnmarshalRead(r.Body, &got))
		assert.Equal(t, map[string]any{"recipient": map[string]any{"comment_id": "100_200_300"}, "message": map[string]any{"text": "Example private reply"}}, got)
		_, _ = fmt.Fprint(w, `{"recipient_id":"opaque-recipient","message_id":"private-301"}`)
	})
	got, err := c.PrivateReply(t.Context(), "100", "100_200_300", "Example private reply")
	require.NoError(t, err)
	assert.Equal(t, PrivateReplyResult{RecipientID: "opaque-recipient", MessageID: "private-301"}, got)
}
func TestPrivateReplyMissingIDsUnknown(t *testing.T) {
	for _, body := range []string{`{}`, `{"recipient_id":"300"}`, `{"message_id":"m1"}`} {
		t.Run(body, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) })
			_, err := c.PrivateReply(t.Context(), "100", "100_200_300", "reply")
			require.Error(t, err)
			assert.Equal(t, ClassUnknown, Classify(err, true))
		})
	}
}
func TestArchiveReplyUsesDeclaredParentAndSyncDedups(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_200_300", "100_200", "Top comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	obj := *comment("100_200_301", "100_200_300", "Page reply")
	obj.From = &Actor{ID: "100", Name: "Example Page"}
	id, err := imp.ArchiveReply(t.Context(), "9", "100", obj, "100_200")
	require.NoError(t, err)
	got := message(t, st, src.ID, "100_200_301")
	assert.Equal(t, id, got.ID)
	assert.Equal(t, message(t, st, src.ID, "100_200_300").ID, got.ReplyToMessageID.Int64)
	assert.True(t, got.IsFromMe)
	f.comments["100_200_300"] = []Object{obj}
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{Full: true})
	require.NoError(t, err)
	assert.Equal(t, id, message(t, st, src.ID, "100_200_301").ID)
	var count int
	require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id=?", src.ID, obj.ID).Scan(&count))
	assert.Equal(t, 1, count)
}
func TestArchiveReplyRefusesMissingParent(t *testing.T) {
	_, _, imp, _ := setupImport(t)
	_, err := imp.ArchiveReply(context.Background(), "9", "100", Object{ID: "100_200_301", CreatedTime: "2026-11-01T12:00:00+0000"}, "100_200")
	require.Error(t, err)
}
