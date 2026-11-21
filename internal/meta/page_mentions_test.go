package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestApplyPageMentionsArchivesMentioningPostsAndComments(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var reads []string
	postText := "another post"
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		reads = append(reads, r.URL.Path)
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/500_600":
			_, _ = fmt.Fprint(w, `{"id":"500_600","message":"visiting Example Page","from":{"id":"500","name":"Example Visitor"}}`)
		case "/v26.0/500_700":
			_, _ = fmt.Fprintf(w, `{"id":"500_700","message":%q,"from":{"id":"500","name":"Example Visitor"}}`, postText)
		case "/v26.0/500_701":
			_, _ = fmt.Fprint(w, `{"id":"500_701","message":"ask Example Page","created_time":"2026-10-01T12:00:00+0000","from":{"id":"501","name":"Example Friend"}}`)
		default:
			assert.Fail("unexpected Graph read", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	src, err := NewImporter(st, c).Register(t.Context(), "9", "100")
	require.NoError(err)
	reads = nil

	require.NoError(ApplyPageMentions(t.Context(), st, c, "9", "100", feedValues(
		`{"item":"post","verb":"add","post_id":"500_600"}`,
		`{"item":"comment","verb":"add","post_id":"500_700","comment_id":"500_701"}`,
		`{"item":"post","verb":"remove","post_id":"500_600"}`,
	)))
	assert.Equal([]string{"/v26.0/500_600", "/v26.0/500_701", "/v26.0/500_700"}, reads)
	post, comment := message(t, st, src.ID, "500_700"), message(t, st, src.ID, "500_701")
	assert.Equal(post.ID, comment.ReplyToMessageID.Int64)
	assert.False(comment.IsFromMe)
	body, err := st.GetMessageBodyText(t.Context(), message(t, st, src.ID, "500_600").ID)
	require.NoError(err)
	assert.Equal("visiting Example Page", body)

	// A post archived as a comment's context is read again when it is
	// itself mentioned, so its current text is kept.
	postText = "edited to mention Example Page"
	reads = nil
	require.NoError(ApplyPageMentions(t.Context(), st, c, "9", "100", feedValues(`{"item":"post","verb":"add","post_id":"500_700"}`)))
	assert.Equal([]string{"/v26.0/500_700"}, reads)
	body, err = st.GetMessageBodyText(t.Context(), post.ID)
	require.NoError(err)
	assert.Equal("edited to mention Example Page", body)
}
