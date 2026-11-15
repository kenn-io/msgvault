package gitea

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncContinuesPastAHiddenReviewPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	f.pageSize = 1
	ann, octo := giteaUser(2002, "ann"), giteaUser(1001, "octo")
	f.issues["acme/widgets#7"] = pull(70, "widgets", 7, "Widgets", "Body.", ann,
		"2026-11-01T08:00:00Z", "2026-11-02T08:00:00Z")
	f.notifications = []map[string]any{notified(1, "Pull", "acme", "widgets", 7, "Widgets", "2026-11-02T08:00:00Z")}
	f.reviews["acme/widgets#7"] = []map[string]any{
		review(7000, "PENDING", "Hidden draft.", ann, 1, "2026-11-01T09:00:00Z"),
		review(7001, "APPROVED", "Looks good.", octo, 1, "2026-11-02T08:00:00Z"),
	}
	f.reviewComments[7001] = []map[string]any{
		reviewComment(701, 7001, "widget.go", 12, 0, "Keep this.", octo, nil, "2026-11-02T07:59:00Z"),
	}
	gitea := f.forge()
	source := newGiteaSource(t, st, gitea)

	summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.Equal(3, summary.Added)
	messages := archived(t, st, source.ID)
	require.Contains(messages, "review:7001")
	assert.True(strings.HasPrefix(messages["review:7001"].Body, "Approved these changes.\n\nLooks good."))
	require.Contains(messages, "comment:701")
	assert.Contains(messages["comment:701"].Body, "Keep this.")
	assert.NotContains(messages, "review:7000")
	log := f.requestLog()
	assert.Len(requestsWith(log, "/settings/api"), 1, "discovery and fetch share the settings read")
	assert.Equal([]string{
		"GET /repos/acme/widgets/pulls/7/reviews?limit=1&page=1",
		"GET /repos/acme/widgets/pulls/7/reviews?limit=1&page=2",
	}, requestsWith(log, "/pulls/7/reviews?"))
}

func TestReconcileFindsUnwatchedThreadUpdates(t *testing.T) {
	for _, change := range []string{"review with code comment", "opening post edit", "plain comment edit"} {
		t.Run(change, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			f := newFakeGitea(t)
			ann, octo := giteaUser(2002, "ann"), giteaUser(1001, "octo")
			f.issues["acme/widgets#7"] = pull(70, "widgets", 7, "Widgets", "Body.", ann,
				"2026-11-01T08:00:00Z", "2026-11-01T09:00:00Z")
			f.comments["acme/widgets#7"] = []map[string]any{
				comment(701, webBase+"acme/widgets/pulls/7", "I looked at this.", octo, "2026-11-01T09:00:00Z"),
			}
			f.activities = []map[string]any{
				commented(1, "comment_pull", "acme", "widgets", 7, "I looked at this.", octo, "2026-11-01T09:00:00Z"),
			}
			source := newGiteaSource(t, st, f.forge())
			_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
			require.NoError(err)
			before := len(f.requestLog())

			if change != "plain comment edit" {
				f.issues["acme/widgets#7"]["updated_at"] = "2026-11-02T08:00:00Z"
			}
			switch change {
			case "opening post edit":
				f.issues["acme/widgets#7"]["body"] = "Corrected body."
			case "plain comment edit":
				c := f.comments["acme/widgets#7"][0]
				c["body"] = "Corrected comment."
				c["updated_at"] = time.Now().UTC().Format(time.RFC3339)
			default:
				f.reviews["acme/widgets#7"] = []map[string]any{
					review(7001, "APPROVED", "", ann, 1, "2026-11-02T08:00:00Z"),
				}
				f.reviewComments[7001] = []map[string]any{
					reviewComment(702, 7001, "widget.go", 12, 0, "New review note.", ann, nil, "2026-11-02T07:59:00Z"),
				}
			}

			summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
			require.NoError(err)
			assert.Equal(1, summary.Found, "only reconciliation can see the update")
			assert.Equal(1, summary.Archived)
			messages := archived(t, st, source.ID)
			switch change {
			case "opening post edit":
				assert.Zero(summary.Added)
				assert.Equal(1, summary.Updated)
				assert.Contains(messages["issue:70"].Body, "Corrected body.")
			case "plain comment edit":
				assert.Zero(summary.Added)
				assert.Equal(1, summary.Updated)
				assert.Contains(messages["comment:701"].Body, "Corrected comment.")
			default:
				assert.Equal(2, summary.Added)
				assert.Zero(summary.Updated)
				assert.Contains(messages, "review:7001")
				require.Contains(messages, "comment:702")
				assert.Contains(messages["comment:702"].Body, "New review note.")
			}
			checks := requestsWith(f.requestLog()[before:], "/comments?since=")
			if change == "plain comment edit" {
				assert.Len(checks, 1, "unchanged issue timestamp falls back to plain comment edits")
			} else {
				assert.Empty(checks, "a changed issue timestamp needs no comment check")
			}
		})
	}
}

func TestACodeCommentReplyRewritesNoOtherPost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	source := newGiteaSource(t, st, f.forge())
	owners := forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}
	_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)
	before := archived(t, st, source.ID)

	f.issues["acme/widgets#7"]["updated_at"] = "2026-11-08T08:00:00Z"
	f.reviews["acme/widgets#7"][0]["comments_count"] = 2
	f.reviewComments[7001] = append(f.reviewComments[7001],
		reviewComment(704, 7001, "widget.go", 12, 0, "Renamed.", giteaUser(2002, "ann"), nil, "2026-11-08T08:00:00Z"))
	f.notifications = append(f.notifications, notified(9, "Pull", "acme", "widgets", 7, "Add widgets", "2026-11-08T08:00:00Z"))

	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)
	assert.Equal(1, summary.Added)
	assert.Zero(summary.Updated, "the review's count is activity, not new review evidence")
	after := archived(t, st, source.ID)
	require.Contains(after, "comment:704")
	assert.Contains(after["comment:704"].Body, "Renamed.")
	for id, post := range before {
		assert.Equal(post, after[id], id)
	}
}

func TestQueuedThreadKeepsItsKeyAfterRepositoryRename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	ann := giteaUser(2002, "ann")
	f.issues["acme/old#7"] = issue(70, "acme", "old", 7, "Widgets", "Body.", ann,
		"2026-11-01T08:00:00Z", "2026-11-01T09:00:00Z")
	f.notifications = []map[string]any{notified(1, "Issue", "acme", "old", 7, "Widgets", "2026-11-01T09:00:00Z")}
	f.limited["/repos/issues/search?created=true"] = 3600
	source := newGiteaSource(t, st, f.forge())

	paused, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.True(paused.Paused)
	assert.Equal(1, paused.Found)
	assert.Equal(1, paused.Remaining)
	assert.Empty(archived(t, st, source.ID))

	moved := issue(70, "acme", "new", 7, "Widgets", "Body.", ann,
		"2026-11-01T08:00:00Z", "2026-11-02T08:00:00Z")
	repository, ok := moved["repository"].(map[string]any)
	require.True(ok)
	repository["id"] = repositoryID("acme/old")
	f.issues["acme/new#7"] = moved
	delete(f.issues, "acme/old#7")
	f.notifications = nil
	f.hits["created"] = []string{"acme/new#7"}

	resumed, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Zero(resumed.Found, "the renamed thread is already queued")
	assert.Zero(resumed.Unavailable)
	assert.Equal(1, resumed.Archived)
	assert.Len(requestsWith(f.requestLog(), "/issues/7"), 2, "one opening post and one comments request")
	assert.Empty(requestsWith(f.requestLog(), "/repos/acme/old/"))
	messages := archived(t, st, source.ID)
	require.Len(messages, 1)
	assert.Equal("issue:70", messages["issue:70"].ConversationID)
	assert.Equal("[acme/new] Widgets (#7)", messages["issue:70"].Conversation)
}

func TestQueuedReviewCommentsResumeFromTheirContinuation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	f.notifications = f.notifications[:1]
	f.hits, f.activities = map[string][]string{}, nil
	f.limited["/pulls/7/reviews/7001/comments"] = 3600
	source := newGiteaSource(t, st, f.forge())

	paused, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.True(paused.Paused)
	assert.Equal(1, paused.Remaining)
	assert.Equal(3, paused.Added, "opening post, plain comment and review are saved")
	before := len(f.requestLog())

	resumed, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Equal(1, resumed.Archived)
	assert.Equal(2, resumed.Added, "the two queued reviews' comments")
	assert.Zero(resumed.Updated)
	log := f.requestLog()[before:]
	assert.Empty(requestsWith(log, "/issues/7"), "the opening post is not fetched again")
	assert.Equal([]string{
		"GET /repos/acme/widgets/pulls/7/reviews/7001/comments",
		"GET /repos/acme/widgets/pulls/7/reviews/7004/comments",
	}, requestsWith(log, "/pulls/7/"))
	messages := archived(t, st, source.ID)
	assert.Len(messages, 5)
	assert.Contains(messages, "comment:702")
	assert.Contains(messages, "comment:703")
}

func TestReconcileFindsACommentEditedDuringAPausedFetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	f.notifications = f.notifications[:1]
	f.hits, f.activities = map[string][]string{}, nil
	f.limited["/pulls/7/reviews?"] = 3600
	source := newGiteaSource(t, st, f.forge())

	paused, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	require.True(paused.Paused)
	assert.Equal(2, paused.Added, "opening post and plain comment precede the reviews")

	// Age the saved first-page read to model an hour-long pause. Keep the
	// real continuation, so the next sync resumes the pending review page.
	checkpoint, err := st.GetLatestCheckpointedSyncContext(t.Context(), source.ID)
	require.NoError(err)
	require.True(checkpoint.CursorBefore.Valid)
	var state map[string]jsontext.Value
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
	var queue []map[string]jsontext.Value
	require.NoError(json.Unmarshal(state["queue"], &queue))
	require.Len(queue, 1)
	readAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	queue[0]["read_at"], err = json.Marshal(readAt)
	require.NoError(err)
	state["queue"], err = json.Marshal(queue)
	require.NoError(err)
	blob, err := json.Marshal(state)
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpointContext(t.Context(), checkpoint.ID, &store.Checkpoint{
		PageToken: string(blob), MessagesProcessed: checkpoint.MessagesProcessed,
		MessagesAdded: checkpoint.MessagesAdded, MessagesUpdated: checkpoint.MessagesUpdated,
		ErrorsCount: checkpoint.ErrorsCount,
	}))

	// The account unwatches the thread, and its existing comment is edited
	// early in the pause, without moving the issue's update time.
	f.notifications = nil
	c := f.comments["acme/widgets#7"][0]
	c["body"] = "Corrected during the pause."
	c["updated_at"] = readAt.Add(20 * time.Minute).Format(time.RFC3339)
	before := len(f.requestLog())
	resumed, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Equal(1, resumed.Archived)
	assert.Empty(requestsWith(f.requestLog()[before:], "/issues/7"), "the first page is not fetched again")
	assert.Contains(archived(t, st, source.ID)["comment:701"].Body, "Thanks!", "the edited comment is still missing")

	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.Equal(1, summary.Found, "reconcile reads back to the first page, before the pause")
	assert.Equal(1, summary.Archived)
	assert.Zero(summary.Added)
	assert.Equal(1, summary.Updated)
	assert.Contains(archived(t, st, source.ID)["comment:701"].Body, "Corrected during the pause.")
}

func TestPaginationFollowsTotalThenLinkThenFullPage(t *testing.T) {
	for _, tc := range []struct {
		name, total, link  string
		page, count, limit int
		more               bool
	}{
		{"empty filtered page", "2", "", 1, 0, 1, true},
		{"total wins over missing next link", "4", `<https://git.example.com?page=1>; rel="prev"`, 1, 1, 1, true},
		{"total reached", "2", `<https://git.example.com?page=3>; rel="next"`, 2, 1, 1, false},
		{"next link without total", "", `<https://git.example.com?page=2>; rel="next"`, 1, 0, 1, true},
		{"last link without total", "", `<https://git.example.com?page=1>; rel="prev"`, 2, 1, 1, false},
		{"full page without metadata", "", "", 1, 1, 1, true},
		{"short page without metadata", "", "", 1, 1, 2, false},
		{"empty page without metadata", "", "", 1, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.total != "" {
				header.Set("X-Total-Count", tc.total)
			}
			if tc.link != "" {
				header.Set("Link", tc.link)
			}
			assert.Equal(t, tc.more, hasMore(header, tc.page, tc.count, tc.limit))
		})
	}
}

func TestAThreadWhoseReconcileCheckFailsIsFetchedAndKept(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	source := newGiteaSource(t, st, f.forge())
	owners := forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}
	_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)

	// The server now fails every read of one archived thread.
	f.fail["/repos/acme/tools/issues/4"] = 500
	gitea := f.forge()
	gitea.reconcile = 10
	summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, owners)
	require.ErrorIs(err, forge.ErrEveryThreadFailed)
	assert.Equal(1, summary.Failed, "a thread whose check failed is fetched, and its failure recorded")
	assert.Equal(1, summary.Remaining, "and kept for the next sync")
}

func TestASameSecondChangeAfterAFetchIsFetchedAgain(t *testing.T) {
	// A reply and an edit made in the same second as the update a sync
	// fetched the thread for move no update time, so a stream reads the
	// thread's entry again unchanged, and the archived-thread check finds
	// no newer time. Each reports it while the fetch read it within the
	// core's margin of that update.
	for _, path := range []string{"stream", "archived-thread check"} {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			f := newFakeGitea(t)
			at := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
			ann := giteaUser(2002, "ann")
			f.issues["acme/widgets#7"] = issue(70, "acme", "widgets", 7, "Widgets", "Body.", ann, at, at)
			f.notifications = []map[string]any{notified(1, "Issue", "acme", "widgets", 7, "Widgets", at)}
			gitea := f.forge()
			if path == "stream" {
				gitea.reconcile = 0
			}
			source := newGiteaSource(t, st, gitea)
			_, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{})
			require.NoError(err)

			f.comments["acme/widgets#7"] = []map[string]any{
				comment(701, webBase+"acme/widgets/issues/7", "Same-second reply.", ann, at),
			}
			f.issues["acme/widgets#7"]["body"] = "Edited in the same second."
			if path != "stream" {
				// Only the archived-thread check can see the thread now.
				f.notifications = nil
			}
			summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{})
			require.NoError(err)
			assert.Equal(1, summary.Archived)
			assert.Equal(1, summary.Added, "the reply")
			assert.Equal(1, summary.Updated, "the edited opening post")
			messages := archived(t, st, source.ID)
			assert.Contains(messages["comment:701"].Body, "Same-second reply.")
			assert.Contains(messages["issue:70"].Body, "Edited in the same second.")
		})
	}
}
