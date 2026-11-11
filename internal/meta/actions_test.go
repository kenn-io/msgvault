package meta

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
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
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
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

func TestImportResumesPastRepliesOfDeletedComment(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	f.comments["100_201"] = []Object{*comment("100_202", "100_201", "Example reply")}
	// Page check, post page, and the comment pages of 100_200 and 100_201;
	// the reply page of 100_202 is left for the next run.
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
	require.NoError(err)
	require.True(sum.Partial)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))

	f.denied["100_202/comments"] = true
	before := len(f.paths)
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "saved traversal below a deleted comment is dropped")
	assert.False(sum.Partial)
	assert.NotContains(f.paths[before:], "/v26.0/100_202/comments")
}

func TestImportResumesPastDeletedPendingComment(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "First comment"), *comment("100_203", "100_200", "Second comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	// A full rescan pauses on the reply page of 100_201, with 100_203 saved
	// for later on the same comment page.
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{Full: true, MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_203").ID)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))

	f.denied["100_203/comments"] = true
	before := len(f.paths)
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "a saved copy of a deleted comment is not traversed")
	assert.False(sum.Partial)
	assert.Contains(f.paths[before:], "/v26.0/100_201/comments", "sibling work continues")
	assert.NotContains(f.paths[before:], "/v26.0/100_203/comments")
}

// purgeDeleted runs `msgvault gc` and checks it purged want rows.
func purgeDeleted(t *testing.T, st *store.Store, want int64) {
	t.Helper()
	plan, err := st.PlanGCContext(t.Context())
	require.NoError(t, err)
	purged, err := st.ExecuteGCContext(t.Context(), plan)
	require.NoError(t, err)
	require.Equal(t, want, purged)
}

func TestImportResumesPastDeletedCommentAfterGC(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))
	purgeDeleted(t, st, 1)

	f.denied["100_201/comments"] = true
	before := len(f.paths)
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "the saved reply frame of a deleted comment is gone after gc")
	assert.False(sum.Partial)
	assert.NotContains(f.paths[before:], "/v26.0/100_201/comments")
}

func TestDropDeletedTraversalWithoutArchiveRow(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	// An unmarked row stands in for gc purging it between the mark and the prune.
	execution, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(err)
	require.NoError(dropDeletedTraversal(t.Context(), st, execution, src, []string{"100_201"}))
	require.NoError(execution.Release())

	f.denied["100_201/comments"] = true
	before := len(f.paths)
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.NotContains(f.paths[before:], "/v26.0/100_201/comments")
}

func TestImportResumesPastDeletedPendingCommentAfterGC(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "First comment"), *comment("100_203", "100_200", "Second comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{Full: true, MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_203").ID)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))
	purgeDeleted(t, st, 1)

	f.denied["100_203/comments"] = true
	before := len(f.paths)
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "a saved copy of a deleted comment is gone after gc")
	assert.False(sum.Partial)
	assert.Contains(f.paths[before:], "/v26.0/100_201/comments", "sibling work continues")
	assert.NotContains(f.paths[before:], "/v26.0/100_203/comments")
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_203"})
	require.NoError(err)
	assert.Zero(ids["100_203"], "the deleted comment is not archived again")
}

// savedFrames lists the parent of every saved traversal frame and the IDs
// pending in it.
func savedFrames(t *testing.T, st *store.Store, src *store.Source) []string {
	t.Helper()
	state, err := loadState(t.Context(), st, src, false)
	require.NoError(t, err)
	var frames []string
	for _, edge := range state.edges() {
		for _, f := range state.Phases[edge].Stack {
			frame := []string{f.Parent + ":"}
			for _, child := range f.Pending {
				frame = append(frame, child.ID)
			}
			frames = append(frames, strings.Join(frame, " "))
		}
	}
	return frames
}

// assertDeclaredThreadGone resumes the sync and checks the deleted comment
// 100_201 and its declared child 100_202 are not read, and absent stays absent.
func assertDeclaredThreadGone(t *testing.T, st *store.Store, f *graphFixture, imp *Importer, src *store.Source, absent ...string) {
	t.Helper()
	assert, require := assert.New(t), require.New(t)
	f.denied["100_201/comments"] = true
	f.denied["100_202/comments"] = true
	before := len(f.paths)
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	for _, path := range f.paths[before:] {
		assert.NotContains(path, "100_201")
		assert.NotContains(path, "100_202")
	}
	ids, err := st.MessageExistsBatchContext(t.Context(), src.ID, absent)
	require.NoError(err)
	assert.Empty(ids, "the deleted comments are not archived again")
}

func TestImportResumesPastDeclaredChildOfDeletedCommentAfterGC(t *testing.T) {
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Parent comment"), *comment("100_202", "100_201", "Declared child")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
	require.NoError(err)
	require.True(sum.Partial)
	require.Equal([]string{"100_200:", "100_202:"}, savedFrames(t, st, src))
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))
	purgeDeleted(t, st, 2)

	assertDeclaredThreadGone(t, st, f, imp, src, "100_201", "100_202")
}

func TestImportDropsPendingDeclaredChildOfDeletedComment(t *testing.T) {
	for _, mode := range []string{"moderated and purged", "marked in the archive"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			st, f, imp, src := setupImport(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Parent comment"), *comment("100_202", "100_201", "Declared child")}
			sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
			require.NoError(err)
			require.True(sum.Partial)
			require.Equal([]string{"100_200: 100_202", "100_201:"}, savedFrames(t, st, src))
			absent := []string{"100_202"}
			if mode == "marked in the archive" {
				marked, err := st.MarkMessageThreadDeletedContext(t.Context(), src.ID, "100_201")
				require.NoError(err)
				require.Equal([]string{"100_201"}, marked)
			} else {
				target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
				require.NoError(err)
				client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
				require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))
				purgeDeleted(t, st, 1)
				absent = append(absent, "100_201")
			}

			assertDeclaredThreadGone(t, st, f, imp, src, absent...)
		})
	}
}

func TestImportDropsPendingDeclaredDescendantsOfDeletedComment(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Parent comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	f.comments["100_200"] = []Object{
		*comment("100_204", "100_200", "Unrelated comment"),
		*comment("100_202", "100_201", "Declared child"),
		*comment("100_206", "100_200", "Sibling comment"),
	}
	f.comments["100_204"] = []Object{*comment("100_205", "100_204", "Unrelated reply"), *comment("100_203", "100_202", "Declared grandchild")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{Full: true, MaxRequests: 4})
	require.NoError(err)
	require.True(sum.Partial)
	require.Equal([]string{"100_200: 100_202", "100_204: 100_203", "100_205:"}, savedFrames(t, st, src))
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))

	for _, id := range []string{"100_201", "100_202", "100_203"} {
		f.denied[id+"/comments"] = true
	}
	before := len(f.paths)
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	for _, path := range f.paths[before:] {
		assert.NotRegexp(`100_20[123]`, path)
	}
	ids, err := st.MessageExistsBatchContext(t.Context(), src.ID, []string{"100_202", "100_203", "100_206"})
	require.NoError(err)
	assert.Contains(ids, "100_206", "the sibling is archived")
	assert.Len(ids, 1, "the deleted comment's descendants are not archived")
}

func TestModerateCommentDeleteWithoutPausedSyncAddsNoRun(t *testing.T) {
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	runs := func() int {
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sync_runs WHERE source_id = ?`, src.ID).Scan(&n))
		return n
	}
	before := runs()
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
	require.NoError(err)
	client, _ := writeFixture(t, http.StatusOK, `{"success":true}`)
	require.NoError(ModerateComment(t.Context(), st, client, target, ActionDelete))
	assert.Equal(t, before, runs())
}

func TestModerateCommentReportsUnrecordedChange(t *testing.T) {
	for _, tt := range []struct {
		action CommentAction
		want   string
	}{
		{ActionHide, "Facebook hid comment 100_201, but msgvault could not record it"},
		{ActionUnhide, "Facebook unhid comment 100_201, but msgvault could not record it"},
		{ActionDelete, "Facebook deleted comment 100_201, but msgvault could not record it"},
	} {
		t.Run(string(tt.action), func(t *testing.T) {
			require := require.New(t)
			st, f, imp, src := setupImport(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "100_201").ID)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_archive_write BEFORE UPDATE ON messages
				BEGIN SELECT RAISE(ABORT, 'injected archive failure'); END`)
			require.NoError(err)

			client, writes := writeFixture(t, http.StatusOK, `{"success":true}`)
			err = ModerateComment(t.Context(), st, client, target, tt.action)
			require.Len(*writes, 1, "Meta applied the change")
			require.ErrorContains(err, tt.want)
			var cause sqlite3.Error
			require.ErrorAs(err, &cause, "the archive error stays wrapped")
			require.ErrorContains(err, "injected archive failure")
		})
	}
}
