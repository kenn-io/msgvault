package meta

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// applyRemoval delivers a webhook change that removes item id from post 100_200.
func applyRemoval(t *testing.T, st *store.Store, imp *Importer, item, id string) {
	t.Helper()
	client, ok := imp.api.(*Client)
	require.True(t, ok)
	change := `{"item":"comment","verb":"remove","post_id":"100_200","comment_id":"` + id + `"}`
	if item != "comment" {
		change = `{"item":"` + item + `","verb":"remove","post_id":"` + id + `"}`
	}
	require.NoError(t, ApplyFeed(t.Context(), st, client, "9", "100", feedValues(change)))
}

// deletedFromSource reports whether each archived ID is marked deleted.
func deletedFromSource(t *testing.T, st *store.Store, src *store.Source, ids ...string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, id := range ids {
		var deleted sql.NullTime
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT deleted_from_source_at FROM messages WHERE id = ?`, message(t, st, src.ID, id).ID).Scan(&deleted))
		got[id] = deleted.Valid
	}
	return got
}

// resumeWithout resumes the paused sync with the replies of each denied ID
// unreadable, as Meta answers for a removed object, and checks the run
// completes without asking for them.
func resumeWithout(t *testing.T, f *graphFixture, imp *Importer, denied ...string) []string {
	t.Helper()
	assert, require := assert.New(t), require.New(t)
	for _, id := range denied {
		f.denied[id+"/comments"] = true
	}
	before := len(f.paths)
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err, "the saved traversal of a removed object is skipped")
	assert.False(sum.Partial)
	for _, path := range f.paths[before:] {
		for _, id := range denied {
			assert.NotContains(path, id+"/comments")
		}
	}
	return f.paths[before:]
}

func TestWebhookCommentRemovalPrunesPausedSyncAfterGC(t *testing.T) {
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	// Page check, post page and comment page; the reply page of 100_201 is
	// left for the next run.
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	require.Equal([]string{"100_200:", "100_201:"}, savedFrames(t, st, src))

	applyRemoval(t, st, imp, "comment", "100_201")
	assert.Equal(t, []string{"100_200:"}, savedFrames(t, st, src))
	purgeDeleted(t, st, 1)
	resumeWithout(t, f, imp, "100_201")
}

func TestWebhookCommentRemovalMarksRepliesAndPrunesThem(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Parent comment")}
	f.comments["100_201"] = []Object{*comment("100_202", "100_201", "Example reply")}
	// The reply page of 100_202 is left for the next run.
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
	require.NoError(err)
	require.True(sum.Partial)
	require.Equal([]string{"100_200:", "100_201:", "100_202:"}, savedFrames(t, st, src))

	applyRemoval(t, st, imp, "comment", "100_201")
	assert.Equal(map[string]bool{"100_200": false, "100_201": true, "100_202": true}, deletedFromSource(t, st, src, "100_200", "100_201", "100_202"))
	var processed, updated int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT messages_processed, messages_updated FROM sync_runs WHERE source_id = ? AND sync_type = ? ORDER BY id DESC LIMIT 1`, src.ID, WebhookSyncType).Scan(&processed, &updated))
	assert.Equal(2, processed, "each marked row counts")
	assert.Equal(2, updated)
	assert.Equal([]string{"100_200:"}, savedFrames(t, st, src))
	purgeDeleted(t, st, 2)
	resumeWithout(t, f, imp, "100_201", "100_202")
}

// A paused sync can hold a saved copy of a comment the archive never got. The
// removal prunes it by ID. When Meta's page no longer lists it, reading the
// saved page again before use drops it too.
func TestWebhookRemovalOfPendingComment(t *testing.T) {
	for _, listed := range []bool{true, false} {
		name := "Meta no longer lists it"
		if listed {
			name = "Meta still lists it"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "First comment"), *comment("100_203", "100_200", "Second comment")}
			sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
			require.NoError(err)
			require.True(sum.Partial)
			require.Equal([]string{"100_200: 100_203", "100_201:"}, savedFrames(t, st, src))

			applyRemoval(t, st, imp, "comment", "100_203")
			if !listed {
				f.comments["100_200"] = f.comments["100_200"][:1]
			}
			paths := resumeWithout(t, f, imp, "100_203")
			assert.Contains(paths, "/v26.0/100_201/comments", "sibling work continues")
			ids, err := st.MessageExistsBatchContext(t.Context(), src.ID, []string{"100_203"})
			require.NoError(err)
			assert.Empty(ids, "the removed comment is not archived")
		})
	}
}

// Removing a post marks only the post deleted, but reading its comments fails
// once it is gone, so the paused sync's traversal inside it is dropped.
func TestWebhookPostRemovalPrunesPausedSyncInsidePost(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Parent comment")}
	f.comments["100_201"] = []Object{*comment("100_202", "100_201", "Example reply")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
	require.NoError(err)
	require.True(sum.Partial)
	require.Equal([]string{"100_200:", "100_201:", "100_202:"}, savedFrames(t, st, src))

	applyRemoval(t, st, imp, "status", "100_200")
	assert.Equal(map[string]bool{"100_200": true, "100_201": false, "100_202": false}, deletedFromSource(t, st, src, "100_200", "100_201", "100_202"))
	assert.Empty(savedFrames(t, st, src))
	purgeDeleted(t, st, 1)
	f.posts = nil
	paths := resumeWithout(t, f, imp, "100_200", "100_201", "100_202")
	assert.NotContains(paths, "/v26.0/100/posts", "the run resumes from the saved progress")
	assert.Equal(map[string]bool{"100_201": false, "100_202": false}, deletedFromSource(t, st, src, "100_201", "100_202"), "the post's comments stay archived")
}

// syncRuns lists the type and status of the source's sync runs in order.
func syncRuns(t *testing.T, st *store.Store, src *store.Source) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), `SELECT sync_type, status FROM sync_runs WHERE source_id = ? ORDER BY id`, src.ID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var runs []string
	for rows.Next() {
		var syncType, status string
		require.NoError(t, rows.Scan(&syncType, &status))
		runs = append(runs, syncType+" "+status)
	}
	require.NoError(t, rows.Err())
	return runs
}

// Webhook runs never hide or replace a paused polling run's progress, and a
// removal saves the pruned progress as a new paused polling run.
func TestWebhookRunsKeepPausedPollingProgress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("401", "100_200", "polled text"), *comment("402", "100_200", "polled text"), *comment("403", "100_200", "polled text")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	// Graph returns two comments a page, so 403 is on the next page.
	require.Equal([]string{"100_200: 402", "401:"}, savedFrames(t, st, src))

	applyRemoval(t, st, imp, "comment", "401")
	// The fixture's cursors are offsets, so 401 stays listed to keep 403 on
	// the next page; the saved page drops it either way.
	f.comments["100_200"][1].Message = "newer text"
	client, ok := imp.api.(*Client)
	require.True(ok)
	require.NoError(ApplyFeed(t.Context(), st, client, "9", "100", feedValues(
		`{"item":"comment","verb":"edited","post_id":"100_200","comment_id":"402"}`)))

	assert.Equal([]string{
		src.SourceType + " paused", WebhookSyncType + " completed",
		src.SourceType + " paused", WebhookSyncType + " completed",
	}, syncRuns(t, st, src))
	assert.Equal([]string{"100_200: 402"}, savedFrames(t, st, src), "the latest paused run holds the pruned progress")

	paths := resumeWithout(t, f, imp, "401")
	assert.False(slices.Contains(paths, "/v26.0/100/posts"), "the resumed run continues inside the saved post")
	body, err := st.GetMessageBodyText(t.Context(), message(t, st, src.ID, "402").ID)
	require.NoError(err)
	assert.Equal("newer text", body)
	message(t, st, src.ID, "403")
}
