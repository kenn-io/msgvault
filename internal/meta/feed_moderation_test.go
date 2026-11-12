package meta

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// metaPage is Meta's side of one Page: post 100_200, comment 100_201 and,
// once msgvault replies, reply 100_202. Hides made through it, or set by the
// test as made elsewhere, are what Graph reports.
type metaPage struct {
	hidden    map[string]bool
	replyText string
	replied   bool
}

func newMetaPage(t *testing.T) (*metaPage, *Client) {
	t.Helper()
	m := &metaPage{hidden: map[string]bool{}, replyText: "Thanks"}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v26.0/")
		switch {
		case r.Method == http.MethodPost && id == "100_201/comments":
			m.replied = true
			_, _ = fmt.Fprint(w, `{"id":"100_202"}`)
		case r.Method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			m.hidden[id] = string(body) == "is_hidden=true"
			_, _ = fmt.Fprint(w, `{"success":true}`)
		case id == "me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case id == "100_201" && r.URL.Query().Get("fields") == "id,is_hidden":
			_, _ = fmt.Fprintf(w, `{"id":"100_201","is_hidden":%t}`, m.hidden["100_201"])
		case id == "100_202" && r.URL.Query().Get("fields") == "id,is_hidden":
			_, _ = fmt.Fprintf(w, `{"id":"100_202","is_hidden":%t}`, m.hidden["100_202"])
		case id == "100_200":
			_, _ = fmt.Fprint(w, `{"id":"100_200","message":"Example post","from":{"id":"100","name":"Example Page"}}`)
		case id == "100_201":
			_, _ = fmt.Fprint(w, `{"id":"100_201","message":"Example comment","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300","name":"Example Customer"}}`)
		case id == "100_202" && m.replied:
			// Meta omits the author of the Page's own reply here.
			_, _ = fmt.Fprintf(w, `{"id":"100_202","message":%q,"created_time":"2026-10-02T12:00:00+0000","parent":{"id":"100_201"}}`, m.replyText)
		default:
			assert.Fail(t, "unexpected Graph request", r.Method+" "+r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	return m, c
}

// setupMetaPage registers the Page and archives comment 100_201 by webhook.
func setupMetaPage(t *testing.T) (*store.Store, *metaPage, *Client, *store.Source) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	m, c := newMetaPage(t)
	src, err := NewImporter(st, c).Register(t.Context(), "9", "100")
	require.NoError(t, err)
	applyCommentChange(t, st, c, "add", "100_201")
	return st, m, c, src
}

func applyCommentChange(t *testing.T, st *store.Store, c *Client, verb, id string) {
	t.Helper()
	change := `{"item":"comment","verb":"` + verb + `","post_id":"100_200","comment_id":"` + id + `"}`
	require.NoError(t, ApplyFeed(t.Context(), st, c, "9", "100", feedValues(change)))
}

// archivedComment reads the archive's owner flag and metadata for id.
func archivedComment(t *testing.T, st *store.Store, src *store.Source, id string) (bool, objectMetadata) {
	t.Helper()
	got := message(t, st, src.ID, id)
	saved, err := st.GetMessageMetadataContext(t.Context(), got.ID)
	require.NoError(t, err)
	var meta objectMetadata
	require.NoError(t, json.Unmarshal([]byte(saved.String), &meta))
	return got.IsFromMe, meta
}

// A webhook write of a reply msgvault posted keeps it the owner's even when
// Graph's read of it omits the author.
func TestWebhookKeepsOutboundReplyTheOwners(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, m, c, src := setupMetaPage(t)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	_, err = ReplyToComment(t.Context(), st, c, target, "Thanks")
	require.NoError(err)
	fromMe, meta := archivedComment(t, st, src, "100_202")
	require.True(fromMe)
	require.True(meta.Outbound)

	m.replyText = "Thanks, edited"
	applyCommentChange(t, st, c, "edited", "100_202")
	body, err := st.GetMessageBodyText(t.Context(), message(t, st, src.ID, "100_202").ID)
	require.NoError(err)
	assert.Equal("Thanks, edited", body)
	fromMe, meta = archivedComment(t, st, src, "100_202")
	assert.True(fromMe, "an edit keeps the reply the owner's")
	assert.True(meta.Outbound)

	for _, hidden := range []bool{true, false} {
		m.hidden["100_202"] = hidden
		verb := map[bool]string{true: "hide", false: "unhide"}[hidden]
		applyCommentChange(t, st, c, verb, "100_202")
		fromMe, meta = archivedComment(t, st, src, "100_202")
		assert.Equal(hidden, meta.Hidden, verb)
		assert.True(meta.Outbound, verb)
		assert.True(fromMe, verb)
	}
}

// `comment moderate` and webhooks record hidden state in the same field, so
// whichever ran last decides it.
func TestWebhookAndModerationShareHiddenState(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, m, c, src := setupMetaPage(t)
	id := message(t, st, src.ID, "100_201").ID
	hidden := func() bool {
		target, err := ResolveCommentTarget(t.Context(), st, id)
		require.NoError(err)
		return target.Hidden
	}
	target, err := ResolveCommentTarget(t.Context(), st, id)
	require.NoError(err)

	require.NoError(ModerateComment(t.Context(), st, c, target, ActionHide))
	require.True(hidden())
	m.hidden["100_201"] = false // unhidden on Meta outside msgvault
	applyCommentChange(t, st, c, "unhide", "100_201")
	assert.False(hidden(), "a webhook unhide after a moderated hide")
	_, meta := archivedComment(t, st, src, "100_201")
	assert.False(meta.Hidden)

	m.hidden["100_201"] = true // hidden on Meta outside msgvault
	applyCommentChange(t, st, c, "hide", "100_201")
	require.True(hidden())
	require.NoError(ModerateComment(t.Context(), st, c, target, ActionUnhide))
	assert.False(hidden(), "a moderated unhide after a webhook hide")
	assert.False(m.hidden["100_201"])
}
