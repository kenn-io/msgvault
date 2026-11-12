package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
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
	rows, err := st.DB().QueryContext(t.Context(), `SELECT source_message_id, json_extract(metadata, '$.kinds') FROM messages WHERE source_id=?`, src.ID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, kind string
		require.NoError(rows.Scan(&id, &kind))
		kinds[id] = kind
	}
	require.NoError(rows.Err())
	assert.Equal(map[string]string{"800": `["caption_mention"]`, "810": `["comment_mention"]`, "901": `["comment_mention"]`}, kinds)
	// A repeated delivery updates the same rows in place.
	require.NoError(ApplyMentions(t.Context(), st, c, "9", "100", "200", feedValues(`{"media_id":"810","comment_id":"901"}`)))
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(3, count)
	root, comment := message(t, st, src.ID, "810"), message(t, st, src.ID, "901")
	assert.Equal(root.ID, comment.ReplyToMessageID.Int64)
	assert.Equal(root.ConversationID, comment.ConversationID)
	assert.Equal(MentionsSourceType, comment.MessageType)
}

// mentionedAndTaggedFixture serves media 700 both as tagged media and as a
// caption mention, and comment 901 on it as a comment mention.
func mentionedAndTaggedFixture(t *testing.T) *Client {
	t.Helper()
	const media = `{"id":"700","caption":"thanks @example_business","timestamp":"2000-01-02T03:04:05+0000","username":"example_visitor"}`
	return fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		fields := r.URL.Query().Get("fields")
		switch {
		case r.URL.Path == "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case r.URL.Path == "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case r.URL.Path == "/v26.0/200/tags":
			_, _ = fmt.Fprint(w, `{"data":[`+media+`]}`)
		case r.URL.Path == "/v26.0/200" && fields == "mentioned_media.media_id(700){"+mentionMediaFields+"}":
			_, _ = fmt.Fprint(w, `{"id":"200","mentioned_media":`+media+`}`)
		case r.URL.Path == "/v26.0/200" && fields == "mentioned_comment.comment_id(901){id,text,timestamp,media{"+mentionMediaFields+"}}":
			_, _ = fmt.Fprint(w, `{"id":"200","mentioned_comment":{"id":"901","text":"ask @example_business","timestamp":"2000-01-03T03:04:05+0000","media":`+media+`}}`)
		default:
			assert.Fail(t, "unexpected Graph read", r.URL.String())
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
}

// mentionMetadataOf returns the archived metadata of the object id.
func mentionMetadataOf(t *testing.T, st *store.Store, source int64, id string) string {
	t.Helper()
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages WHERE source_id=? AND source_message_id=?`, source, id).Scan(&metadata))
	return metadata
}

// Media that is both tagged and caption-mentioned records both ways, whichever
// writer runs first, so neither writer keeps rewriting the other's row.
func TestMentionKindsDoNotDependOnWriteOrder(t *testing.T) {
	for _, tt := range []struct {
		name     string
		tagFirst bool
	}{{"tag then caption mention", true}, {"caption mention then tag", false}} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			c := mentionedAndTaggedFixture(t)
			imp := NewMentionImporter(st, c)
			src, err := imp.Register(t.Context(), "9", "100", "200")
			require.NoError(err)
			tag := func() {
				_, err := imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
				require.NoError(err)
			}
			mention := func() {
				require.NoError(ApplyMentions(t.Context(), st, c, "9", "100", "200", feedValues(`{"media_id":"700"}`)))
			}
			if tt.tagFirst {
				tag()
				mention()
			} else {
				mention()
				tag()
			}
			assert.JSONEq(`{"app_id":"9","page_id":"100","instagram_account_id":"200","media_id":"700","kinds":["caption_mention","tag"]}`,
				mentionMetadataOf(t, st, src.ID, "700"))

			sum, err := imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
			require.NoError(err)
			assert.Equal(1, sum.MessagesProcessed)
			assert.Zero(sum.MessagesAdded)
			assert.Zero(sum.MessagesUpdated, "a repeated tag sync leaves the row as it is")
		})
	}
}

// A comment mention on media that tag sync archived first archives only the
// comment; the media row, and so its kinds, stay as tag sync wrote them.
func TestCommentMentionLeavesTaggedMediaKinds(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := mentionedAndTaggedFixture(t)
	imp := NewMentionImporter(st, c)
	src, err := imp.Register(t.Context(), "9", "100", "200")
	require.NoError(err)
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	require.NoError(ApplyMentions(t.Context(), st, c, "9", "100", "200", feedValues(`{"media_id":"700","comment_id":"901"}`)))

	assert.JSONEq(`{"app_id":"9","page_id":"100","instagram_account_id":"200","media_id":"700","kinds":["tag"]}`,
		mentionMetadataOf(t, st, src.ID, "700"))
	assert.JSONEq(`{"app_id":"9","page_id":"100","instagram_account_id":"200","media_id":"700","comment_id":"901","kinds":["comment_mention"]}`,
		mentionMetadataOf(t, st, src.ID, "901"))
}

// Graph answering a mention read with a different object is answered the same
// way on every retry, so the change is skipped at once and nothing is archived.
func TestApplyMentionsSkipsMentionGraphAnswersWithOtherObject(t *testing.T) {
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
			_, _ = fmt.Fprint(w, `{"id":"200","mentioned_media":{"id":"899","caption":"thanks @example_business","username":"example_visitor"}}`)
		case r.URL.Path == "/v26.0/200" && fields == "mentioned_comment.comment_id(901){id,text,timestamp,media{"+mentionMediaFields+"}}":
			_, _ = fmt.Fprint(w, `{"id":"200","mentioned_comment":{"id":"999","text":"ask @example_business","media":{"id":"810","username":"example_friend"}}}`)
		default:
			assert.Fail("unexpected Graph read", r.URL.String())
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	src, err := NewMentionImporter(st, c).Register(t.Context(), "9", "100", "200")
	require.NoError(err)

	require.ErrorIs(ApplyMentions(t.Context(), st, c, "9", "100", "200", feedValues(`{"media_id":"800"}`)), ErrInvalidChange)
	require.ErrorIs(ApplyMentions(t.Context(), st, c, "9", "100", "200", feedValues(`{"media_id":"810","comment_id":"901"}`)), ErrInvalidChange)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Zero(count)
}
