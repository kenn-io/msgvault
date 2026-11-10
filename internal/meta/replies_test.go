package meta

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// replyFixture is Meta's side of a reply: the send, then the read of the new
// comment. postStatus and getStatus pick each answer.
type replyFixture struct {
	postStatus, getStatus int
	posted, read          string
	requests              []graphWrite
}

func newReplyFixture(t *testing.T, posted, read string) (*replyFixture, *Client) {
	t.Helper()
	f := &replyFixture{postStatus: http.StatusOK, getStatus: http.StatusOK, posted: posted, read: read}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		f.requests = append(f.requests, graphWrite{r.Method, r.URL.Path, string(body)})
		status, response := f.postStatus, f.posted
		if r.Method == http.MethodGet {
			status, response = f.getStatus, f.read
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, response)
	})
	return f, c
}

func TestReplyToComment(t *testing.T) {
	for _, tt := range []struct {
		name, comment, replyID, posted, read string
		archive                              func(t *testing.T, st *store.Store) *store.Source
		wantRequests                         []graphWrite
	}{
		{"facebook", "100_201", "100_202", `{"id":"100_202"}`,
			`{"id":"100_202","message":"Thanks for asking","created_time":"2026-10-02T12:00:00+0000","from":{"id":"100","name":"Example Page"},"parent":{"id":"100_201"}}`,
			archiveFacebookComment, []graphWrite{
				{http.MethodPost, "/v26.0/100_201/comments", "message=Thanks+for+asking"},
				{http.MethodGet, "/v26.0/100_202", ""},
			}},
		{"instagram", "400", "600", `{"id":"600"}`,
			`{"id":"600","text":"Thanks for asking","timestamp":"2026-10-02T12:00:00+0000","parent_id":"400"}`,
			archiveInstagramComment, []graphWrite{
				{http.MethodPost, "/v26.0/400/replies", "message=Thanks+for+asking"},
				{http.MethodGet, "/v26.0/600", ""},
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			src := tt.archive(t, st)
			parent := message(t, st, src.ID, tt.comment)
			target, err := ResolveCommentTarget(t.Context(), st, parent.ID)
			require.NoError(err)
			f, client := newReplyFixture(t, tt.posted, tt.read)

			replyID, err := ReplyToComment(t.Context(), st, client, target, "Thanks for asking")
			require.NoError(err)
			assert.Equal(tt.replyID, replyID)
			assert.Equal(tt.wantRequests, f.requests)
			reply := message(t, st, src.ID, tt.replyID)
			assert.Equal(parent.ID, reply.ReplyToMessageID.Int64)
			assert.Equal(parent.ConversationID, reply.ConversationID)
			body, err := st.GetMessageBodyText(t.Context(), reply.ID)
			require.NoError(err)
			assert.Equal("Thanks for asking", body)

			_, err = ReplyToComment(t.Context(), st, client, target, "Thanks for asking")
			require.ErrorIs(err, ErrReplySent)
			assert.Len(f.requests, 2, "an identical reply is not sent twice")
		})
	}
}

func TestReplyToCommentOutcomes(t *testing.T) {
	const posted = `{"id":"100_202"}`
	const read = `{"id":"100_202","message":"Thanks","from":{"id":"100","name":"Example Page"},"parent":{"id":"100_201"}}`
	setup := func(t *testing.T) (*store.Store, CommentTarget, *replyFixture, *Client) {
		t.Helper()
		st := testutil.NewSQLiteTestStore(t)
		src := archiveFacebookComment(t, st)
		target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
		require.NoError(t, err)
		f, client := newReplyFixture(t, posted, read)
		return st, target, f, client
	}
	posts := func(f *replyFixture) int {
		n := 0
		for _, request := range f.requests {
			if request.method == http.MethodPost {
				n++
			}
		}
		return n
	}

	t.Run("lost answer blocks a resend until cleared", func(t *testing.T) {
		st, target, f, client := setup(t)
		f.postStatus = http.StatusBadGateway
		_, err := ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorIs(t, err, ErrReplyUnknown)
		f.postStatus = http.StatusOK
		_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorIs(t, err, ErrReplyUnknown)
		assert.Equal(t, 1, posts(f), "the unknown reply blocks an identical send")
		_, err = ReplyToComment(t.Context(), st, client, target, "Thanks, again")
		require.NoError(t, err, "a different reply is not blocked")

		require.NoError(t, ClearUnknownReply(t.Context(), st, target, "Thanks"))
		_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.NoError(t, err)
		assert.Equal(t, 3, posts(f))
		require.Error(t, ClearUnknownReply(t.Context(), st, target, "Thanks"), "a sent reply cannot be cleared")
	})
	t.Run("refused reply can be sent again", func(t *testing.T) {
		st, target, f, client := setup(t)
		f.postStatus = http.StatusBadRequest
		_, err := ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorAs(t, err, new(*APIError))
		require.NotErrorIs(t, err, ErrReplyUnknown)
		f.postStatus = http.StatusOK
		_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.NoError(t, err)
		assert.Equal(t, 2, posts(f))
	})
	t.Run("refusal in an HTTP 200 body can be sent again", func(t *testing.T) {
		st, target, f, client := setup(t)
		f.posted = `{"error":{"code":368,"message":"synthetic"}}`
		_, err := ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorAs(t, err, new(*APIError))
		require.NotErrorIs(t, err, ErrReplyUnknown)
		f.posted = posted
		_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.NoError(t, err)
		assert.Equal(t, 2, posts(f))
	})
	t.Run("top-level target is the parent when Meta omits it", func(t *testing.T) {
		st, target, f, client := setup(t)
		f.read = `{"id":"100_202","message":"Thanks","from":{"id":"100","name":"Example Page"}}`
		_, err := ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.NoError(t, err)
		assert.Equal(t, target.MessageID, message(t, st, target.SourceID, "100_202").ReplyToMessageID.Int64)
	})
	t.Run("posted reply that cannot be recorded says so", func(t *testing.T) {
		st, target, f, client := setup(t)
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER block_reply_finish BEFORE UPDATE ON meta_comment_replies
			BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`)
		require.NoError(t, err)
		replyID, err := ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorContains(t, err, "reply 100_202 was posted")
		require.ErrorContains(t, err, "check the comment on Facebook before sending it again")
		assert.Equal(t, "100_202", replyID)
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER block_reply_finish`)
		require.NoError(t, err)
		_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorIs(t, err, ErrReplyUnknown, "the unrecorded reply still blocks a resend")
		assert.Equal(t, 1, posts(f))
	})
	t.Run("posted reply that cannot be read back still counts as sent", func(t *testing.T) {
		st, target, f, client := setup(t)
		f.getStatus = http.StatusInternalServerError
		replyID, err := ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorContains(t, err, "not archived")
		assert.Equal(t, "100_202", replyID)
		_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
		require.ErrorIs(t, err, ErrReplySent)
		assert.Equal(t, 1, posts(f))
	})
}

func TestReplyArchiveMatchesLaterSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	_, client := newReplyFixture(t, `{"id":"100_202"}`,
		`{"id":"100_202","message":"Thanks","created_time":"2026-10-02T12:00:00+0000","from":{"id":"100","name":"Example Page"},"parent":{"id":"100_201"}}`)
	_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
	require.NoError(err)

	f.comments["100_201"] = []Object{{ID: "100_202", Message: "Thanks", CreatedTime: "2026-10-02T12:00:00+0000", From: &Actor{ID: "100", Name: "Example Page"}, Parent: &Actor{ID: "100_201"}}}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "sync accepts the reply msgvault archived")
	assert.Zero(sum.MessagesAdded)
	assert.True(message(t, st, src.ID, "100_202").IsFromMe)
}

func TestReplyToNestedFacebookCommentWaitsForSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	f.comments["100_201"] = []Object{*comment("100_203", "100_201", "Nested comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_203").ID)
	require.NoError(err)
	_, client := newReplyFixture(t, `{"id":"100_204"}`, `{"id":"100_204","message":"Thanks","from":{"id":"100","name":"Example Page"}}`)

	replyID, err := ReplyToComment(t.Context(), st, client, target, "Thanks")
	require.ErrorIs(err, ErrReplyNotArchived)
	assert.Equal("100_204", replyID)
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_204"})
	require.NoError(err)
	assert.Zero(ids["100_204"], "a reply without a known parent is left for sync")
	_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
	require.ErrorIs(err, ErrReplySent, "the reply still counts as sent")

	f.comments["100_201"] = append(f.comments["100_201"], Object{ID: "100_204", Message: "Thanks", CreatedTime: "2026-10-02T12:00:00+0000", From: &Actor{ID: "100", Name: "Example Page"}, Parent: &Actor{ID: "100_201"}})
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.Equal(message(t, st, src.ID, "100_201").ID, message(t, st, src.ID, "100_204").ReplyToMessageID.Int64)
}

func TestInstagramReplyStaysFromMe(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, c := newInstagramImportFixture(t)
	imp := NewImporter(st, c)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "400").ID)
	require.NoError(err)
	replies, client := newReplyFixture(t, `{"id":"600"}`, "")
	replies.getStatus = http.StatusInternalServerError

	_, err = ReplyToComment(t.Context(), st, client, target, "Thanks")
	require.ErrorIs(err, ErrReplyNotArchived)
	f.replies = append(f.replies, IGComment{ID: "600", Text: "Thanks", Timestamp: "2026-10-02T12:00:00+0000", ParentID: "400", From: &IGCommentFromUser{ID: "700", Username: "example_business"}})
	for range 2 {
		_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
		require.NoError(err)
		assert.True(message(t, st, src.ID, "600").IsFromMe, "the reply msgvault posted stays the owner's")
	}
	assert.False(message(t, st, src.ID, "500").IsFromMe, "other comments keep unproven ownership")
}
