package github

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/testutil"
)

func review(id, state, body string, author map[string]any, comments map[string]any) map[string]any {
	return map[string]any{
		"id": id, "url": "https://github.com/r/" + id, "body": body, "state": state,
		"submittedAt": "2026-11-07T08:30:00Z", "createdAt": "2026-11-07T08:29:00Z", "author": author, "comments": comments,
	}
}

func reviewComment(id, body string, author map[string]any) map[string]any {
	return map[string]any{
		"id": id, "url": "https://github.com/rc/" + id, "body": body, "path": "widget.go", "line": 3,
		"originalLine": 3, "createdAt": "2026-11-07T08:29:00Z", "replyTo": nil, "author": author,
	}
}

func discussionComment(id, body string, author, replies map[string]any) map[string]any {
	return map[string]any{
		"id": id, "url": "https://github.com/dc/" + id, "body": body, "createdAt": "2026-11-05T07:00:00Z",
		"isAnswer": false, "author": author, "replies": replies,
	}
}

func TestEveryNestedListContinuesOnItsLaterPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	ann := user("ann", 2002)
	f.issueSearch = []searchNode{found("PullRequest", "acme", "widgets", 9, "2026-11-07T09:00:00Z")}
	f.discussionSearch = []searchNode{found("Discussion", "acme", "widgets", 5, "2026-11-06T09:00:00Z")}
	f.threads["acme/widgets#9"] = map[string]any{
		"__typename": "PullRequest", "id": "PR_9", "number": 9, "title": "Long review", "url": "https://github.com/acme/widgets/pull/9",
		"body": "Body.", "createdAt": "2026-11-06T10:00:00Z", "author": ann, "comments": conn(),
		"reviews": connMore("r1", review("PRR_A", "COMMENTED", "First pass.", ann, connMore("rc1", reviewComment("RC_1", "One.", ann)))),
	}
	f.pages["MoreReviews PR_9 r1"] = conn(review("PRR_B", "APPROVED", "", ann, conn()))
	f.pages["MoreReviewComments PRR_A rc1"] = conn(reviewComment("RC_2", "Two.", ann))
	f.discussions["acme/widgets#5"] = map[string]any{
		"id": "D_5", "number": 5, "title": "Long talk", "url": "https://github.com/acme/widgets/discussions/5",
		"body": "Body.", "createdAt": "2026-11-05T06:00:00Z", "author": ann,
		"comments": connMore("dc1", discussionComment("DC_1", "One.", ann, connMore("rp1", map[string]any{
			"id": "R_1", "url": "https://github.com/rp/1", "body": "Reply one.", "createdAt": "2026-11-05T08:00:00Z", "replyTo": map[string]any{"id": "DC_1"}, "author": ann,
		}))),
	}
	f.pages["MoreDiscussionComments D_5 dc1"] = conn(discussionComment("DC_2", "Two.", ann, conn()))
	f.pages["MoreReplies DC_1 rp1"] = conn(map[string]any{
		"id": "R_2", "url": "https://github.com/rp/2", "body": "Reply two.", "createdAt": "2026-11-05T09:00:00Z", "replyTo": map[string]any{"id": "DC_1"}, "author": ann,
	})
	source := newGitHubSource(t, st)

	summary, err := syncGitHub(t, st, f, source, forge.Options{})
	require.NoError(err)
	assert.Equal(2, summary.Archived)
	messages := archived(t, st, source.ID)
	for _, id := range []string{"PR_9", "PRR_A", "RC_1", "RC_2", "PRR_B", "D_5", "DC_1", "DC_2", "R_1", "R_2"} {
		assert.Contains(messages, id, "a post on a later page of its list")
	}
}

func TestAuthorsBecomeAccountsGhostsAndMigratedNames(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	f.scopes = "notifications, repo, read:user"
	withEmail := user("ann", 2002)
	withEmail["email"] = "ann@example.com"
	robot := map[string]any{"__typename": "Bot", "login": "ci-bot", "url": "https://github.com/apps/ci-bot", "databaseId": 3003, "email": "bot@example.com"}
	f.issueSearch = []searchNode{found("Issue", "acme", "widgets", 4, "2026-11-07T09:00:00Z")}
	f.threads["acme/widgets#4"] = map[string]any{
		"__typename": "Issue", "id": "I_4", "number": 4, "title": "Who wrote this", "url": "https://github.com/acme/widgets/issues/4",
		"body": "Body.", "createdAt": "2026-11-06T10:00:00Z", "author": nil,
		"comments": conn(
			comment("IC_M", "Imported.", "2026-11-06T11:00:00Z", map[string]any{"__typename": "Mannequin", "login": "old-user", "url": "https://github.com/old-user"}),
			comment("IC_B", "CI passed.", "2026-11-06T12:00:00Z", robot),
			comment("IC_A", "Me too.", "2026-11-06T13:00:00Z", withEmail),
		),
	}
	source := newGitHubSource(t, st)

	_, err := syncGitHub(t, st, f, source, forge.Options{})
	require.NoError(err)
	messages := archived(t, st, source.ID)
	assert.Equal("url:https://github.com/ghost", messages["I_4"].SenderValue, "a deleted account is GitHub's ghost")
	assert.True(strings.HasPrefix(messages["IC_M"].SenderValue, "name:"), "a migrated name is no account")
	assert.Equal("github.com:user:3003", messages["IC_B"].SenderValue, "a bot never joins a person by email")
	var email string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COALESCE(p.email_address, '') FROM messages m JOIN participants p ON p.id = m.sender_id
		WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "IC_A").Scan(&email))
	assert.Equal("ann@example.com", email, "an account with a public email joins that person")
	assert.Equal("github.com:user:2002", messages["IC_A"].SenderValue, "and keeps its GitHub identity")
}

func TestIncludeOwnersLimitsWhatIsFetched(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)

	summary, err := syncGitHub(t, st, f, source, forge.Options{Owners: forge.NewOwnerFilter([]string{"acme"}, nil)})
	require.NoError(err)
	assert.Equal(t, 4, summary.Archived)
	for _, request := range f.requestLog() {
		if strings.HasPrefix(request, "SearchThreads") {
			assert.Contains(t, request, "user:acme")
			continue
		}
		assert.NotContains(t, strings.ToLower(request), "secret-corp", "only included owners' threads are fetched")
	}
}

func TestAuthorErrorsDecideBetweenAGhostAndARetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	f.notifications = nil
	source := newGitHubSource(t, st)
	// GitHub withholds one author: the post is the ghost's.
	f.threads["acme/widgets#7"]["comments"] = conn(comment("IC_1", "Thanks!", "2026-11-07T08:00:00Z", nil))
	f.gqlErrors["IssueThread acme/widgets#7"] = []map[string]any{{"type": "FORBIDDEN", "message": "Resource not accessible",
		"path": []any{"repository", "issueOrPullRequest", "comments", "nodes", 0, "author"}}}
	// A timeout at another author leaves its thread incomplete.
	f.gqlErrors["IssueThread acme/gadgets#3"] = []map[string]any{{"type": "SERVICE_UNAVAILABLE", "message": "Timeout",
		"path": []any{"repository", "issueOrPullRequest", "comments", "nodes", 0, "author"}}}

	summary, err := syncGitHub(t, st, f, source, forge.Options{})
	require.NoError(err)
	assert.Equal(1, summary.Failed, "the timed-out thread is fetched again later")
	messages := archived(t, st, source.ID)
	assert.Equal("url:https://github.com/ghost", messages["IC_1"].SenderValue)
	assert.NotContains(messages, "I_3")
}

func TestADiscussionBothStreamsFindIsFetchedOnce(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	f.discussionSearch = []searchNode{found("Discussion", "acme", "widgets", 9, "2026-11-05T08:00:00Z")}
	source := newGitHubSource(t, st)

	_, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Equal(t, 1, countPrefix(f.requestLog(), "DiscussionThread acme/widgets#9"))
}

func TestAPullRequestTheAccountOnlyReviewedIsFound(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	f.reviewedSearch = []searchNode{found("PullRequest", "acme", "widgets", 11, "2026-11-02T09:00:00Z")}
	f.threads["acme/widgets#11"] = map[string]any{
		"__typename": "PullRequest", "id": "PR_11", "number": 11, "title": "Reviewed only", "url": "https://github.com/acme/widgets/pull/11",
		"body": "Body.", "createdAt": "2026-11-01T10:00:00Z", "author": user("ann", 2002), "comments": conn(),
		"reviews": conn(review("PRR_11", "APPROVED", "", user("octo", 1001), conn())),
	}
	source := newGitHubSource(t, st)

	_, err := syncGitHub(t, st, f, source, forge.Options{})
	require.NoError(err)
	assert.Contains(t, archived(t, st, source.ID), "PRR_11")
}

func TestAFailedDiscussionLookupHoldsOnlyThatNotification(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	f.issueSearch = nil
	f.fail["RecentDiscussions"] = true
	source := newGitHubSource(t, st)

	first, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Equal(2, first.Archived, "the pull request and the release still sync")
	assert.NotContains(archived(t, st, source.ID), "D_9")

	delete(f.fail, "RecentDiscussions")
	second, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Contains(archived(t, st, source.ID), "D_9", "the held notification is read again")
	assert.Positive(second.Archived)
}

func TestADiscussionNoLongerListedIsSkipped(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	f.notifications = []notification{notified("Discussion", "acme", "widgets", "Deleted long ago", "", "mention", "2026-11-05T08:00:00Z")}
	source := newGitHubSource(t, st)

	summary, err := syncGitHub(t, st, f, source, forge.Options{})
	require.NoError(err)
	assert.Zero(t, summary.Remaining, "a title the repository's list doesn't hold is no thread to queue")
}
