package tiktok

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCommentSyncResumesDeduplicatesAndLinksReplies(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	paths := []string{}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/business/get/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example-account","display_name":"Example Account"}}`)
		case "/business/video/list/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"videos":[{"item_id":9007199254740993,"caption":"Example video","create_time":1700000000}],"has_more":false}}`)
		case "/business/comment/list/":
			if r.URL.Query().Get("cursor") == "7" {
				_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[{"comment_id":9007199254740995,"video_id":9007199254740993,"text":"Example comment","unique_identifier":"actor-example","display_name":"Example Viewer","replies":1,"create_time":1700000001}],"has_more":false}}`)
			} else {
				// Empty pages with has_more are valid; do not treat these as done.
				_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[],"cursor":7,"has_more":true}}`)
			}
		case "/business/comment/reply/list/":
			assertions.Equal("9007199254740995", r.URL.Query().Get("comment_id"))
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[{"comment_id":9007199254740997,"parent_comment_id":9007199254740995,"video_id":9007199254740993,"text":"Example reply","unique_identifier":"actor-example","display_name":"Example Viewer","create_time":1700000002},{"comment_id":"9007199254740997","parent_comment_id":"9007199254740995","video_id":"9007199254740993","text":"Example reply","unique_identifier":"actor-example","create_time":1700000002}],"has_more":false}}`)
		default:
			http.NotFound(w, r)
		}
	})
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	var added int
	var complete bool
	for range 10 {
		sum, err := imp.Import(t.Context(), "app-example", "account-example", ImportOptions{MaxRequests: 2})
		requirements.NoError(err)
		added += sum.MessagesAdded
		if !sum.Partial {
			complete = true
			break
		}
	}
	requirements.True(complete)
	assertions.Equal(3, added)
	count, err := st.CountMessagesForSource(src.ID)
	requirements.NoError(err)
	assertions.EqualValues(3, count)
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, commentFormat, []string{"video:9007199254740993", "9007199254740995", "9007199254740997"})
	requirements.NoError(err)
	requirements.Len(rows, 3)
	var replyParent, commentParent int64
	requirements.NoError(st.DB().QueryRow("SELECT reply_to_message_id FROM messages WHERE id = ?", rows["9007199254740997"].ID).Scan(&replyParent))
	requirements.NoError(st.DB().QueryRow("SELECT reply_to_message_id FROM messages WHERE id = ?", rows["9007199254740995"].ID).Scan(&commentParent))
	assertions.Equal(rows["9007199254740995"].ID, replyParent)
	assertions.Equal(rows["video:9007199254740993"].ID, commentParent)
	assertions.Equal(rows["9007199254740995"].SenderID, rows["9007199254740997"].SenderID)
	assertions.True(rows["video:9007199254740993"].SourceIsFromMe.Bool)
	assertions.Contains(paths, "/business/comment/reply/list/")
	sum, err := imp.Import(t.Context(), "app-example", "account-example", ImportOptions{})
	requirements.NoError(err)
	assertions.False(sum.Partial)
	assertions.Zero(sum.MessagesAdded)
}

func TestCommentSyncRejectsLoopingCursor(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/business/get/" {
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"videos":[],"cursor":0,"has_more":true}}`)
	})
	imp := NewImporter(st, c)
	_, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	sum, err := imp.Import(t.Context(), "app-example", "account-example", ImportOptions{})
	requirements.ErrorContains(err, "cursor")
	assert.True(t, sum.Partial)
}
