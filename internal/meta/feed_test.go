package meta

import (
	"database/sql"
	"encoding/json/jsontext"
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
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		reads = append(reads, r.URL.Path)
		switch r.URL.Path {
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

	reply := `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_202","parent_id":"100_201","message":"webhook reply","created_time":1790000000,"from":{"id":"301","name":"Example Visitor"}}`
	reaction := `{"item":"reaction","verb":"add","post_id":"100_200","reaction_type":"like"}`
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(reaction, reply)))
	assert.Equal([]string{"/v26.0/100_201", "/v26.0/100_200"}, reads)
	post, parent, got := message(t, st, src.ID, "100_200"), message(t, st, src.ID, "100_201"), message(t, st, src.ID, "100_202")
	assert.Equal(post.ID, parent.ReplyToMessageID.Int64)
	assert.Equal(parent.ID, got.ReplyToMessageID.Int64)
	assert.Equal(post.ConversationID, got.ConversationID)
	body, err := st.GetMessageBodyText(t.Context(), got.ID)
	require.NoError(err)
	assert.Equal("webhook reply", body)

	reads = nil
	edit := strings.Replace(strings.Replace(reply, `"add"`, `"edited"`, 1), "webhook reply", "edited reply", 1)
	require.NoError(ApplyFeed(t.Context(), st, c, "9", "100", feedValues(edit,
		`{"item":"comment","verb":"hide","post_id":"100_200","comment_id":"100_202","parent_id":"100_201"}`,
		`{"item":"comment","verb":"remove","post_id":"100_200","comment_id":"100_201","parent_id":"100_200"}`)))
	assert.Empty(reads, "archived ancestors are not read again")
	body, err = st.GetMessageBodyText(t.Context(), got.ID)
	require.NoError(err)
	assert.Equal("edited reply", body)
	metadata, err := st.GetMessageMetadataContext(t.Context(), got.ID)
	require.NoError(err)
	assert.Contains(metadata.String, `"hidden":true`)
	var deleted sql.NullTime
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT deleted_from_source_at FROM messages WHERE id=?`, parent.ID).Scan(&deleted))
	assert.True(deleted.Valid)

	var webhookRuns int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sync_runs WHERE source_id=? AND sync_type=? AND status='completed'`, src.ID, WebhookSyncType).Scan(&webhookRuns))
	assert.Equal(2, webhookRuns)
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
	require.NoError(ApplyFeed(t.Context(), st, nil, "9", "100", feedValues(edit)))

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
