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

type graphWrite struct{ method, path, body string }

// writeFixture is the Meta side of a comment change. It records each request
// and confirms it.
func writeFixture(t *testing.T) (*Client, *[]graphWrite) {
	t.Helper()
	var writes []graphWrite
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		writes = append(writes, graphWrite{r.Method, r.URL.Path, string(body)})
		_, _ = fmt.Fprint(w, `{"success":true}`)
	})
	return c, &writes
}

// archiveFacebookComment syncs comment 100_201 on post 100_200.
func archiveFacebookComment(t *testing.T, st *store.Store) *store.Source {
	t.Helper()
	f, c := newGraphFixture(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "9", "100")
	require.NoError(t, err)
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	return src
}

// archiveInstagramComment syncs comment 400 on media 300.
func archiveInstagramComment(t *testing.T, st *store.Store) *store.Source {
	t.Helper()
	_, c := newInstagramImportFixture(t)
	imp := NewImporter(st, c)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(t, err)
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(t, err)
	return src
}

func TestModerateComment(t *testing.T) {
	for _, tt := range []struct {
		name, comment, hideParam string
		archive                  func(t *testing.T, st *store.Store) *store.Source
	}{
		{"facebook", "100_201", "is_hidden", archiveFacebookComment},
		{"instagram", "400", "hide", archiveInstagramComment},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			src := tt.archive(t, st)
			id := message(t, st, src.ID, tt.comment).ID
			target, err := ResolveCommentTarget(t.Context(), st, id)
			require.NoError(err)
			assert.Equal(tt.comment, target.CommentID)
			assert.False(target.Hidden)

			client, writes := writeFixture(t)
			require.NoError(ModerateComment(t.Context(), st, client, target, ActionHide))
			target, err = ResolveCommentTarget(t.Context(), st, id)
			require.NoError(err)
			assert.True(target.Hidden)

			require.NoError(ModerateComment(t.Context(), st, client, target, ActionUnhide))
			target, err = ResolveCommentTarget(t.Context(), st, id)
			require.NoError(err)
			assert.False(target.Hidden)

			require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))
			_, err = ResolveCommentTarget(t.Context(), st, id)
			require.ErrorIs(err, store.ErrMessageNotFound)
			var deleted bool
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`, id).Scan(&deleted))
			assert.True(deleted, "the archive keeps the deleted comment")

			assert.Equal([]graphWrite{
				{http.MethodPost, "/v26.0/" + tt.comment, tt.hideParam + "=true"},
				{http.MethodPost, "/v26.0/" + tt.comment, tt.hideParam + "=false"},
				{http.MethodDelete, "/v26.0/" + tt.comment, ""},
			}, *writes)
		})
	}
}

func TestModerateCommentDeleteMarksReplies(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Parent comment"), *comment("100_203", "100_200", "Sibling comment")}
	f.comments["100_201"] = []Object{*comment("100_202", "100_201", "First reply")}
	f.comments["100_202"] = []Object{*comment("100_204", "100_202", "Nested reply")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	ids := map[string]int64{}
	for _, id := range []string{"100_200", "100_201", "100_202", "100_203", "100_204"} {
		ids[id] = message(t, st, src.ID, id).ID
	}
	require.Equal(ids["100_202"], message(t, st, src.ID, "100_204").ReplyToMessageID.Int64)
	target, err := ResolveCommentTarget(t.Context(), st, ids["100_201"])
	require.NoError(err)
	client, _ := writeFixture(t)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))

	for id, want := range map[string]bool{"100_200": false, "100_201": true, "100_202": true, "100_203": false, "100_204": true} {
		var deleted bool
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`, ids[id]).Scan(&deleted))
		assert.Equal(want, deleted, id)
	}
	for id, want := range map[string]string{"100_201": "Parent comment", "100_202": "First reply", "100_204": "Nested reply"} {
		text, err := st.GetMessageBodyText(t.Context(), ids[id])
		require.NoError(err)
		assert.Equal(want, text, "the archive keeps the text of deleted %s", id)
	}
}

func TestImportResumesPastDeletedComment(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	// Page check, post page and comment page; the reply page of 100_201 is
	// left for the next run.
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	client, _ := writeFixture(t)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))

	f.denied["100_201/comments"] = true
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "the saved reply frame of a deleted comment is skipped")
	assert.False(sum.Partial)
	assert.NotContains(f.paths, "/v26.0/100_201/comments")
}
