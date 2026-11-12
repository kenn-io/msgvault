package meta

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func feedValues(values ...string) []jsontext.Value {
	out := make([]jsontext.Value, len(values))
	for i, value := range values {
		out[i] = jsontext.Value(value)
	}
	return out
}

func TestApplyFeedArchivesCommentsAndState(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var reads []string
	replyText := "graph reply"
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		reads = append(reads, r.URL.Path)
		switch r.URL.Path {
		case "/v26.0/100_202":
			if r.URL.Query().Get("fields") == "id,is_hidden" {
				reads[len(reads)-1] += " is_hidden"
				_, _ = fmt.Fprint(w, `{"id":"100_202","is_hidden":true}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":"100_202","message":%q,"created_time":"2026-10-01T12:30:00+0000","from":{"id":"301","name":"Example Visitor"},"parent":{"id":"100_201"}}`, replyText)
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100_200":
			_, _ = fmt.Fprint(w, `{"id":"100_200","message":"Example post","from":{"id":"100","name":"Example Page"}}`)
		case "/v26.0/100_201":
			// A top-level comment has no parent field.
			_, _ = fmt.Fprint(w, `{"id":"100_201","message":"first","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300","name":"Example Customer"}}`)
		default:
			assert.Fail("unexpected Graph read", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	src, err := NewImporter(st, c).Register(t.Context(), "9", "100")
	require.NoError(err)
	reads = nil

	reply := `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_202","parent_id":"100_201","message":"delivery text","created_time":1790000000,"from":{"id":"301","name":"Example Visitor"}}`
	reaction := `{"item":"reaction","verb":"add","post_id":"100_200","reaction_type":"like"}`
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(reaction, reply)))
	assert.Equal([]string{"/v26.0/100_202", "/v26.0/100_201", "/v26.0/100_200"}, reads)
	post, parent, got := message(t, st, src.ID, "100_200"), message(t, st, src.ID, "100_201"), message(t, st, src.ID, "100_202")
	assert.Equal(post.ID, parent.ReplyToMessageID.Int64)
	assert.Equal(parent.ID, got.ReplyToMessageID.Int64)
	assert.Equal(post.ConversationID, got.ConversationID)
	body, err := st.GetMessageBodyText(t.Context(), got.ID)
	require.NoError(err)
	assert.Equal("graph reply", body, "the comment is read from Graph, not taken from the delivery")

	reads = nil
	replyText = "edited reply"
	edit := strings.Replace(reply, `"add"`, `"edited"`, 1)
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(edit,
		`{"item":"comment","verb":"hide","post_id":"100_200","comment_id":"100_202","parent_id":"100_201"}`,
		`{"item":"comment","verb":"remove","post_id":"100_200","comment_id":"100_201","parent_id":"100_200"}`)))
	assert.Equal([]string{"/v26.0/100_202", "/v26.0/100_202 is_hidden"}, reads, "archived ancestors are not read again")
	body, err = st.GetMessageBodyText(t.Context(), got.ID)
	require.NoError(err)
	assert.Equal("edited reply", body)
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(reply)))
	body, err = st.GetMessageBodyText(t.Context(), got.ID)
	require.NoError(err)
	assert.Equal("edited reply", body, "a repeated older delivery keeps the current text")
	metadata, err := st.GetMessageMetadataContext(t.Context(), got.ID)
	require.NoError(err)
	assert.Contains(metadata.String, `"hidden":true`)
	var deleted sql.NullTime
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT deleted_from_source_at FROM messages WHERE id=?`, parent.ID).Scan(&deleted))
	assert.True(deleted.Valid)

	var webhookRuns int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sync_runs WHERE source_id=? AND sync_type=? AND status='completed'`, src.ID, WebhookSyncType).Scan(&webhookRuns))
	assert.Equal(3, webhookRuns)
}

// Hidden state is read from Graph, so a hide that Meta delivers again after
// an unhide cannot hide a visible comment.
func TestApplyFeedHiddenStateFollowsGraph(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	hidden := false
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case r.URL.Path == "/v26.0/100_200":
			_, _ = fmt.Fprint(w, `{"id":"100_200","message":"Example post","from":{"id":"100","name":"Example Page"}}`)
		case r.URL.Path == "/v26.0/100_201" && r.URL.Query().Get("fields") == "id,is_hidden":
			_, _ = fmt.Fprintf(w, `{"id":"100_201","is_hidden":%t}`, hidden)
		case r.URL.Path == "/v26.0/100_201":
			_, _ = fmt.Fprint(w, `{"id":"100_201","message":"first","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300","name":"Example Customer"}}`)
		default:
			assert.Fail("unexpected Graph read", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	src, err := NewImporter(st, c).Register(t.Context(), "9", "100")
	require.NoError(err)
	change := func(verb string) string {
		return `{"item":"comment","verb":"` + verb + `","post_id":"100_200","comment_id":"100_201","parent_id":"100_200"}`
	}
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(change("add"))))
	id := message(t, st, src.ID, "100_201").ID
	archivedHidden := func() bool {
		saved, err := st.GetMessageMetadataContext(t.Context(), id)
		require.NoError(err)
		var meta objectMetadata
		require.NoError(json.Unmarshal([]byte(saved.String), &meta))
		return meta.Hidden
	}

	hide, unhide := change("hide"), change("unhide")
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(hide)))
	assert.False(archivedHidden(), "a hide records the visibility Graph reports")

	hidden = true
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(unhide)))
	assert.True(archivedHidden(), "an unhide records the visibility Graph reports")

	hidden = false
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(unhide)))
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(hide)))
	assert.False(archivedHidden(), "a hide delivered again after the unhide leaves the comment visible")
}

// An archive invariant that the same change always breaks makes the change
// invalid, so the webhook worker skips it instead of retrying it.
func TestApplyFeedReportsArchiveInvariantsAsInvalidChanges(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v26.0/")
		switch {
		case id == "me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case id == "100_200" || id == "100_900":
			_, _ = fmt.Fprintf(w, `{"id":%q,"message":"Example post","from":{"id":"100","name":"Example Page"}}`, id) // #nosec G705 -- test fixture; id is a path segment matched above and is JSON-quoted.
		case id == "100_201":
			_, _ = fmt.Fprint(w, `{"id":"100_201","message":"first","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300","name":"Example Customer"}}`)
		case id == "100_400":
			// Graph answers with a different comment than the one asked for.
			_, _ = fmt.Fprint(w, `{"id":"100_401","message":"other","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300","name":"Example Customer"}}`)
		case strings.HasPrefix(id, "100_3"):
			// Each reply in this chain answers the one after it, past the read limit.
			n, err := strconv.Atoi(strings.TrimPrefix(id, "100_3"))
			if !assert.NoError(err) {
				http.Error(w, "unexpected", http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"message":"reply","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300","name":"Example Customer"},"parent":{"id":"100_3%02d"}}`, id, n+1) // #nosec G705 -- test fixture; id is a path segment matched above and is JSON-quoted.
		default:
			assert.Fail("unexpected Graph read", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	src, err := NewImporter(st, c).Register(t.Context(), "9", "100")
	require.NoError(err)
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(
		`{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201"}`)))

	err = ApplyFeed(t.Context(), st, c, "9", "100", feedValues(
		`{"item":"comment","verb":"add","post_id":"100_900","comment_id":"100_201"}`))
	require.ErrorIs(err, ErrInvalidChange, "a comment archived under another post")
	assert.Contains(err.Error(), "comment object already belongs to a different root or parent")

	err = ApplyFeed(t.Context(), st, c, "9", "100", feedValues(
		`{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_300"}`))
	require.ErrorIs(err, ErrInvalidChange, "a reply chain longer than the read limit")
	assert.Contains(err.Error(), "comment ancestors exceed the read limit")

	err = ApplyFeed(t.Context(), st, c, "9", "100", feedValues(
		`{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_400"}`))
	require.ErrorIs(err, ErrInvalidChange, "Graph answered with a different comment")
	assert.Contains(err.Error(), "graph object identity mismatch")
	ids, err := st.MessageExistsBatchContext(t.Context(), src.ID, []string{"100_400", "100_401"})
	require.NoError(err)
	assert.Empty(ids, "neither comment is archived")
}

func TestApplyFeedRefusesUnregisteredPage(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	reply := `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_202","parent_id":"100_200","message":"x"}`
	require.ErrorIs(t, ApplyFeed(t.Context(), st, nil, "9", "100", feedValues(reply)), store.ErrSourceNotFound)
}

// A polling run paused by its budget resumes after a webhook run: it keeps its
// place instead of starting over, and it archives a fresh read of the page it
// saved, so the newer webhook text is not replaced by the saved copy.
func TestPollingResumeKeepsPlaceAndRereadsSavedPage(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	for i := 1; i <= 3; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(strconv.Itoa(400+i), "100_200", "polled text"))
	}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	message(t, st, src.ID, "401")

	f.comments["100_200"][1].Message = "newer text"
	edit := `{"item":"comment","verb":"edited","post_id":"100_200","comment_id":"402","parent_id":"100_200","message":"newer text","from":{"id":"300","name":"Example Customer"}}`
	client, ok := imp.api.(*Client)
	require.True(ok)
	require.NoError(ApplyFeed(t.Context(), st, client, "9", "100", feedValues(edit)))

	f.paths = nil
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.False(slices.Contains(f.paths, "/v26.0/100/posts"), "the resumed run continues inside the saved post")
	body, err := st.GetMessageBodyText(t.Context(), message(t, st, src.ID, "402").ID)
	require.NoError(err)
	assert.Equal("newer text", body)
	message(t, st, src.ID, "403")
}

// Reading a saved page again costs a request, but only when traversal is about
// to use it, so a run with one request after verification still advances.
func TestPollingResumeAdvancesWithSmallBudget(t *testing.T) {
	st, f, imp, src := setupImport(t)
	for i := 1; i <= 3; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(strconv.Itoa(400+i), "100_200", "small budget"))
	}
	completed := false
	for range 40 {
		sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 2})
		require.NoError(t, err)
		if !sum.Partial {
			completed = true
			break
		}
	}
	require.True(t, completed)
	for i := 1; i <= 3; i++ {
		message(t, st, src.ID, strconv.Itoa(400+i))
	}
}
