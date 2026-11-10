package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestApplyMentionsArchivesCaptionAndCommentMentions(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		fields := r.URL.Query().Get("fields")
		switch {
		case r.URL.Path == "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case r.URL.Path == "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case r.URL.Path == "/v26.0/200" && fields == "mentioned_media.media_id(800){"+mentionMediaFields+"}":
			_, _ = fmt.Fprint(w, `{"id":"200","mentioned_media":{"id":"800","caption":"thanks @example_business","username":"example_visitor"}}`)
		case r.URL.Path == "/v26.0/200" && fields == "mentioned_comment.comment_id(901){id,text,timestamp,media{"+mentionMediaFields+"}}":
			_, _ = fmt.Fprint(w, `{"id":"200","mentioned_comment":{"id":"901","text":"ask @example_business","timestamp":"2000-01-02T03:04:05+0000","media":{"id":"810","caption":"a friend's post","username":"example_friend"}}}`)
		default:
			assert.Fail("unexpected Graph read", r.URL.String())
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	src, err := NewMentionImporter(st, c).Register(t.Context(), "9", "100", "200")
	require.NoError(err)

	require.ErrorIs(ApplyMentions(t.Context(), st, c, "9", "100", "200", feedValues(`{"media_id":"811","comment_id":"901"}`)), ErrInvalidChange,
		"a comment on other media than the change names is refused")
	require.NoError(ApplyMentions(t.Context(), st, c, "9", "100", "200", feedValues(`{"media_id":"800"}`, `{"media_id":"810","comment_id":"901"}`)))

	kinds := map[string]string{}
	rows, err := st.DB().QueryContext(t.Context(), `SELECT source_message_id, json_extract(metadata, '$.kind') FROM messages WHERE source_id=?`, src.ID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, kind string
		require.NoError(rows.Scan(&id, &kind))
		kinds[id] = kind
	}
	require.NoError(rows.Err())
	assert.Equal(map[string]string{"800": "caption_mention", "810": "comment_mention", "901": "comment_mention"}, kinds)
	root, comment := message(t, st, src.ID, "810"), message(t, st, src.ID, "901")
	assert.Equal(root.ID, comment.ReplyToMessageID.Int64)
	assert.Equal(root.ConversationID, comment.ConversationID)
	assert.Equal(MentionsSourceType, comment.MessageType)
}
