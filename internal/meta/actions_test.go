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
// and answers with the given status and body.
func writeFixture(t *testing.T, status int, response string) (*Client, *[]graphWrite) {
	t.Helper()
	var writes []graphWrite
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		writes = append(writes, graphWrite{r.Method, r.URL.Path, string(body)})
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, response)
	})
	return c, &writes
}

func TestModerateComment(t *testing.T) {
	for _, tt := range []struct {
		name, comment, hideParam string
		archive                  func(t *testing.T, st *store.Store) *store.Source
	}{
		{"facebook", "100_201", "is_hidden", func(t *testing.T, st *store.Store) *store.Source {
			t.Helper()
			f, c := newGraphFixture(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
			imp := NewImporter(st, c)
			src, err := imp.Register(t.Context(), "9", "100")
			require.NoError(t, err)
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(t, err)
			return src
		}},
		{"instagram", "400", "hide", func(t *testing.T, st *store.Store) *store.Source {
			t.Helper()
			_, c := newInstagramImportFixture(t)
			imp := NewImporter(st, c)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(t, err)
			_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
			require.NoError(t, err)
			return src
		}},
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

			client, writes := writeFixture(t, http.StatusOK, `{"success":true}`)
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

func TestModerateCommentSurvivesSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	id := message(t, st, src.ID, "100_201").ID
	target, err := ResolveCommentTarget(t.Context(), st, id)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionHide))

	f.comments["100_200"][0].Message = "Edited comment"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	target, err = ResolveCommentTarget(t.Context(), st, id)
	require.NoError(err)
	assert.Equal("Edited comment", target.Text)
	assert.True(target.Hidden, "a later sync keeps the hide made through msgvault")
}

func TestModerateCommentRefusals(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	id := message(t, st, src.ID, "100_201").ID
	target, err := ResolveCommentTarget(t.Context(), st, id)
	require.NoError(t, err)
	hidden := func(t *testing.T) bool {
		t.Helper()
		current, err := ResolveCommentTarget(t.Context(), st, id)
		require.NoError(t, err)
		return current.Hidden
	}

	t.Run("post is not a comment", func(t *testing.T) {
		_, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_200").ID)
		require.ErrorContains(t, err, "not a comment")
	})
	t.Run("Meta rejects the change", func(t *testing.T) {
		client, _ := writeFixture(t, http.StatusForbidden, `{"error":{"code":200}}`)
		err := ModerateComment(t.Context(), st, client, target, ActionHide)
		var apiErr *APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 200, apiErr.Code)
		assert.False(t, hidden(t))
	})
	t.Run("Meta does not confirm the change", func(t *testing.T) {
		client, _ := writeFixture(t, http.StatusOK, `{"success":false}`)
		require.Error(t, ModerateComment(t.Context(), st, client, target, ActionHide))
		assert.False(t, hidden(t))
	})
	t.Run("sync running", func(t *testing.T) {
		execution, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
		require.NoError(t, err)
		defer func() { assert.NoError(t, execution.Release()) }()
		client, writes := writeFixture(t, http.StatusOK, `{"success":true}`)
		require.ErrorIs(t, ModerateComment(t.Context(), st, client, target, ActionHide), store.ErrSyncAlreadyActive)
		assert.Empty(t, *writes, "Meta is not called while a sync owns the source")
	})
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
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))

	f.denied["100_201/comments"] = true
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "the saved reply frame of a deleted comment is skipped")
	assert.False(sum.Partial)
	assert.NotContains(f.paths, "/v26.0/100_201/comments")
}
