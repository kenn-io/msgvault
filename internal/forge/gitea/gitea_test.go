package gitea

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// accountID is the account the fake's token belongs to.
const accountID = "1001"

func newGiteaSource(t *testing.T, st *store.Store, gitea *Forge) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSourceContext(t.Context(), Name, forge.SourceIdentifier(gitea.Instance(), accountID))
	require.NoError(t, err)
	// add-gitea confirms the account, so its posts are its own.
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), source.ID, forge.AccountKey(gitea.Instance(), accountID), "account-identifier"))
	return source
}

// seedThreads gives the fake a thread of every kind the account takes part
// in, found through every stream, and threads it must leave alone.
func seedThreads(f *fakeGitea) {
	octo, ann, robot := giteaUser(1001, "octo"), giteaUser(2002, "ann"), giteaBot(3003, "ci-bot")

	// acme/widgets#7: ann's pull request, which octo reviews.
	f.issues["acme/widgets#7"] = pull(70, "widgets", 7, "Add widgets", "Adds widgets.", ann, "2026-11-06T10:00:00Z", "2026-11-07T09:00:00Z")
	f.comments["acme/widgets#7"] = []map[string]any{comment(701, webBase+"acme/widgets/pulls/7", "Thanks!", octo, "2026-11-07T08:00:00Z")}
	f.reviews["acme/widgets#7"] = []map[string]any{
		review(7001, "APPROVED", "LGTM", octo, 1, "2026-11-07T08:30:00Z"),
		review(7002, "PENDING", "draft", octo, 1, "2026-11-07T08:40:00Z"),
		review(7003, "REQUEST_REVIEW", "", giteaUser(4004, "bob"), 0, "2026-11-07T08:41:00Z"),
		review(7004, "COMMENT", "", ann, 1, "2026-11-07T08:50:00Z"),
	}
	f.reviewComments[7001] = []map[string]any{reviewComment(702, 7001, "widget.go", 12, 0, "Nit: rename.", octo, nil, "2026-11-07T08:29:00Z")}
	f.reviewComments[7002] = []map[string]any{reviewComment(705, 7002, "widget.go", 2, 0, "Draft note.", octo, nil, "2026-11-07T08:39:00Z")}
	f.reviewComments[7004] = []map[string]any{reviewComment(703, 7004, "old.go", 0, 3, "Why remove this?", ann, octo, "2026-11-07T08:49:00Z")}

	// acme/widgets#8: an issue by a deleted account, with a migrated comment.
	f.issues["acme/widgets#8"] = issue(80, "acme", "widgets", 8, "Old bug", "From a deleted account.", ghost(), "2020-01-01T10:00:00Z", "2026-11-06T09:00:00Z")
	f.comments["acme/widgets#8"] = []map[string]any{comment(801, webBase+"acme/widgets/issues/8", "Carried over.", migratedAuthor("old-name"), "2020-01-02T10:00:00Z")}

	// acme/gadgets#3: an issue octo opened, which nobody notified it about.
	f.issues["acme/gadgets#3"] = issue(30, "acme", "gadgets", 3, "Gadgets break", "They break.", octo, "2026-10-01T08:00:00Z", "2026-10-02T08:00:00Z")
	f.comments["acme/gadgets#3"] = []map[string]any{
		comment(301, webBase+"acme/gadgets/issues/3", "Can you share a log?", ann, "2026-10-01T09:00:00Z"),
		comment(302, webBase+"acme/gadgets/issues/3", "CI passed.", robot, "2026-10-02T08:00:00Z"),
	}

	// acme/tools#4: an issue octo only commented on.
	f.issues["acme/tools#4"] = issue(40, "acme", "tools", 4, "Tools are slow", "Slow.", ann, "2026-09-01T08:00:00Z", "2026-09-02T08:00:00Z")
	f.comments["acme/tools#4"] = []map[string]any{comment(401, webBase+"acme/tools/issues/4", "I looked into it.", octo, "2026-09-02T08:00:00Z")}

	// An excluded owner's threads.
	f.issues["secret-corp/plans#1"] = issue(91, "secret-corp", "plans", 1, "Plans", "Secret.", ann, "2026-11-05T08:00:00Z", "2026-11-05T09:00:00Z")
	f.issues["secret-corp/plans#2"] = issue(92, "secret-corp", "plans", 2, "More plans", "Secret.", octo, "2026-09-01T08:00:00Z", "2026-09-03T08:00:00Z")

	commit := notified(3, "Commit", "acme", "widgets", 0, "0f1e2d", "2026-11-05T08:00:00Z")
	commit["subject"] = map[string]any{
		"title": "0f1e2d", "url": webBase + "acme/widgets/commit/0f1e2d", "html_url": webBase + "acme/widgets/commit/0f1e2d",
		"latest_comment_url": "", "latest_comment_html_url": "", "type": "Commit", "state": "",
	}
	f.notifications = []map[string]any{
		notified(1, "Pull", "acme", "widgets", 7, "Add widgets", "2026-11-07T09:00:00Z"),
		notified(2, "Issue", "acme", "widgets", 8, "Old bug", "2026-11-06T09:00:00Z"),
		commit,
		notified(4, "Issue", "secret-corp", "plans", 1, "Plans", "2026-11-05T09:00:00Z"),
		// The account lost access to this one's repository
		// (services/convert/notification.go:26-38).
		{"id": 5, "repository": nil, "subject": nil, "unread": false, "pinned": false, "updated_at": "2026-11-04T09:00:00Z", "url": webBase + "api/v1/notifications/threads/5"},
	}
	f.hits["created"] = []string{"acme/gadgets#3", "secret-corp/plans#2"}
	f.hits["review_requested"] = []string{"acme/widgets#7"}
	created := commented(13, "create_repo", "acme", "new", 0, "", octo, "2026-09-04T08:00:00Z")
	created["content"] = ""
	f.activities = []map[string]any{
		commented(11, "comment_issue", "acme", "tools", 4, "I looked into it.", octo, "2026-09-02T08:00:00Z"),
		commented(12, "comment_issue", "secret-corp", "plans", 2, "Secret.", octo, "2026-09-03T08:00:00Z"),
		created,
	}
}

type archivedMessage struct {
	Kind, Subject, Conversation, ConversationID string
	FromMe                                      bool
	SenderType, SenderValue, Body               string
	Metadata                                    map[string]any
	Raw                                         string
}

func archived(t *testing.T, st *store.Store, sourceID int64) map[string]archivedMessage {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT m.id, m.source_message_id, COALESCE(m.subject, ''), c.title, c.source_conversation_id, m.is_from_me,
		       COALESCE((SELECT identifier_type FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), ''),
		       COALESCE((SELECT identifier_value FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), ''),
		       COALESCE(mb.body_text, ''), m.metadata
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_id = ?`), sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]archivedMessage{}
	ids := map[string]int64{}
	for rows.Next() {
		var (
			id       int64
			key      string
			m        archivedMessage
			metadata string
		)
		require.NoError(t, rows.Scan(&id, &key, &m.Subject, &m.Conversation, &m.ConversationID, &m.FromMe,
			&m.SenderType, &m.SenderValue, &m.Body, &metadata))
		var decoded struct {
			ProviderMetadata map[string]any `json:"provider_metadata"`
		}
		require.NoError(t, json.Unmarshal([]byte(metadata), &decoded))
		m.Metadata = decoded.ProviderMetadata
		m.Kind, _ = m.Metadata["kind"].(string)
		out[key], ids[key] = m, id
	}
	require.NoError(t, rows.Err())
	for key, id := range ids {
		raw, err := st.GetMessageRawContext(t.Context(), id)
		require.NoError(t, err)
		m := out[key]
		m.Raw = string(raw)
		out[key] = m
	}
	return out
}

func requestsWith(log []string, part string) []string {
	var out []string
	for _, request := range log {
		if strings.Contains(request, part) {
			out = append(out, request)
		}
	}
	return out
}

func TestSyncArchivesEveryThreadTheAccountTakesPartIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	gitea := f.forge()
	source := newGiteaSource(t, st, gitea)

	summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{
		Owners: forge.NewOwnerFilter(nil, []string{"Secret-Corp"}),
	})
	require.NoError(err)

	assert.Equal("octo", summary.Login)
	assert.Equal(4, summary.Found, "a notification, a search hit, both, and an activity")
	assert.Equal(4, summary.Archived)
	assert.Equal(12, summary.Added)
	assert.Zero(summary.Remaining)
	assert.Empty(requestsWith(f.requestLog(), "secret-corp"), "nothing of the excluded owner is requested")
	assert.Empty(requestsWith(f.requestLog(), "/reviews/7002/"), "a pending review is a draft")

	messages := archived(t, st, source.ID)
	kinds := map[string]string{}
	for id, m := range messages {
		kinds[id] = m.Kind
	}
	assert.Equal(map[string]string{
		"issue:70": postPullRequest, "comment:701": postIssueComment, "review:7001": postReview,
		"comment:702": postReviewComment, "comment:703": postReviewComment,
		"issue:80": postIssue, "comment:801": postIssueComment,
		"issue:30": postIssue, "comment:301": postIssueComment, "comment:302": postIssueComment,
		"issue:40": postIssue, "comment:401": postIssueComment,
	}, kinds, "every post but drafts, review requests and empty comment reviews")

	pr := messages["issue:70"]
	assert.Equal("Add widgets", pr.Subject)
	assert.Equal("[acme/widgets] Add widgets (#7)", pr.Conversation, "named as Gitea's emails name it")
	assert.Equal("issue:70", messages["comment:703"].ConversationID, "the conversation is the issue's ID")
	assert.Equal("[acme/gadgets] Gadgets break (#3)", messages["comment:302"].Conversation)
	// The archive's text body ends with the post's link.
	assert.True(strings.HasPrefix(messages["review:7001"].Body, "Approved these changes.\n\nLGTM\n\n"))
	assert.Equal("approved", messages["review:7001"].Metadata["review_state"])
	assert.True(strings.HasPrefix(messages["comment:702"].Body, "`widget.go` line 12\n\nNit: rename.\n\n"))
	assert.InDelta(12, messages["comment:702"].Metadata["line"], 0)
	assert.Equal(false, messages["comment:702"].Metadata["resolved"])
	assert.True(strings.HasPrefix(messages["comment:703"].Body, "`old.go` line 3\n\nWhy remove this?\n\n"), "a comment on a removed line")
	assert.Equal(true, messages["comment:703"].Metadata["resolved"])
	assert.Equal("review:7004", messages["comment:703"].Metadata["review"])
	assert.Equal("acme/widgets", messages["comment:703"].Metadata["repository"])
	assert.InDelta(7, messages["comment:703"].Metadata["number"], 0)

	instance := string(gitea.Instance())
	assert.True(messages["comment:701"].FromMe, "the account's own comment")
	assert.False(pr.FromMe)
	assert.Equal(Name, messages["issue:70"].SenderType)
	assert.Equal(instance+":user:2002", messages["issue:70"].SenderValue, "ann is keyed by instance and ID")
	assert.Equal(instance+":user:3003", messages["comment:302"].SenderValue, "a bot is an account")
	assert.NotEqual(Name, messages["issue:80"].SenderType, "a deleted account gets no Gitea identity")
	assert.NotEqual(Name, messages["comment:801"].SenderType, "a migrated name gets no Gitea identity")
	for _, m := range messages {
		assert.NotEqual("email", m.SenderType, "no placeholder email is taken")
	}

	var raw map[string]jsontext.Value
	require.NoError(json.Unmarshal([]byte(pr.Raw), &raw))
	assert.NotContains(raw, "labels", "evidence keeps no lists")
	assert.NotContains(raw, "assets")
	assert.Contains(raw, "pull_request")
	assert.JSONEq(`"Adds widgets."`, string(raw["body"]))
}

func TestDiscoveryContinuesEachStreamFromItsCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	source := newGiteaSource(t, st, f.forge())
	owners := forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}
	_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)
	first := len(f.requestLog())

	ann := giteaUser(2002, "ann")
	// ann replies on #8, which notifies octo again.
	f.notifications[1]["updated_at"] = "2026-11-07T10:00:00Z"
	f.issues["acme/widgets#8"]["updated_at"] = "2026-11-07T10:00:00Z"
	f.comments["acme/widgets#8"] = append(f.comments["acme/widgets#8"],
		comment(802, webBase+"acme/widgets/issues/8", "Still broken.", ann, "2026-11-07T10:00:00Z"))
	// ann assigns octo a new issue, updated within the overlap of the last
	// search's newest update.
	f.issues["acme/gadgets#5"] = issue(50, "acme", "gadgets", 5, "Assigned", "Please look.", ann, "2026-10-02T07:58:00Z", "2026-10-02T07:58:00Z")
	f.hits["assigned"] = []string{"acme/gadgets#5"}
	f.hits["created"] = append(f.hits["created"], "acme/gadgets#5")
	// octo comments on a thread no other stream shows.
	f.issues["acme/tools#6"] = issue(60, "acme", "tools", 6, "Docs", "Docs.", ann, "2026-09-05T08:00:00Z", "2026-09-06T08:00:00Z")
	f.comments["acme/tools#6"] = []map[string]any{comment(601, webBase+"acme/tools/issues/6", "Done.", giteaUser(1001, "octo"), "2026-09-06T08:00:00Z")}
	f.activities = append(f.activities, commented(14, "comment_issue", "acme", "tools", 6, "Done.", giteaUser(1001, "octo"), "2026-09-06T08:00:00Z"))

	second, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)
	assert.Equal(3, second.Found, "only the threads that changed: what the overlap reads again is skipped")
	assert.Equal(3, second.Archived)
	assert.Equal(4, second.Added, "the reply, and the two new threads' posts")

	log := f.requestLog()[first:]
	assert.Equal([]string{"GET /notifications?all=true&limit=50&page=1&since=2026-11-07T08%3A55%3A00Z"},
		requestsWith(log, "/notifications?all=true"), "notifications since the newest the last scan read, less the overlap")
	assert.Equal([]string{"GET /repos/issues/search?created=true&limit=50&page=1&since=2026-10-02T07%3A55%3A00Z&state=all"},
		requestsWith(log, "created=true"), "the search reads back past an issue the indexer may not have seen yet")
	assert.Equal([]string{"GET /repos/issues/search?assigned=true&limit=50&page=1&state=all"},
		requestsWith(log, "assigned=true"), "a stream that found nothing yet reads everything")
	assert.Len(requestsWith(log, "/activities/feeds"), 1, "the feed is read until the activity the last scan reached")
}

func TestIncludedOwnersAreSearchedOwnerByOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	gitea := f.forge()
	source := newGiteaSource(t, st, gitea)

	summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{
		Owners: forge.NewOwnerFilter([]string{"ACME"}, nil),
	})
	require.NoError(err)
	assert.Equal(4, summary.Archived)
	log := f.requestLog()
	searches := requestsWith(log, "/repos/issues/search?")
	assert.Len(searches, len(searchFlags)+1, "one search per flag, beside the scope check")
	for _, search := range searches[1:] {
		assert.Contains(search, "owner=acme", "the search asks only for the included owner's threads")
	}
	assert.Empty(requestsWith(log, "secret-corp"))
}

func TestSyncDropsAThreadWhoseRepositoryMovedToAnExcludedOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	f.repoOwners["acme/widgets"] = "secret-corp"
	gitea := f.forge()
	source := newGiteaSource(t, st, gitea)

	summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{
		Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"}),
	})
	require.NoError(err)
	assert.Equal(2, summary.Excluded, "both of the moved repository's threads")
	assert.Equal(2, summary.Archived)
	assert.Empty(requestsWith(f.requestLog(), "/repos/acme/widgets/"), "the moved repository's threads are never read")
}

func TestThreadResumesFromItsContinuationAfterARateLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	f.notifications = f.notifications[:1]
	f.hits, f.activities = map[string][]string{}, nil
	f.pageSize = 2
	// A proxy in front of the server limits the second page of reviews for
	// an hour; the first page's limit lasts no time and is waited out.
	f.limited["pulls/7/reviews?limit=2&page=1"] = 0
	f.limited["pulls/7/reviews?limit=2&page=2"] = 3600
	source := newGiteaSource(t, st, f.forge())

	paused, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err, "a rate limit pauses the run")
	assert.True(paused.Paused)
	require.NotNil(paused.RateLimit)
	assert.WithinDuration(time.Now().Add(time.Hour), paused.RateLimit.Reset, time.Minute)
	assert.Equal(1, paused.Remaining)
	assert.Equal(4, paused.Added, "the pull request, its comment, the first review and its comment")
	pausedAt := len(f.requestLog())

	resumed, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Equal(1, resumed.Archived)
	assert.Equal(1, resumed.Added, "the second page's review comment")
	log := f.requestLog()[pausedAt:]
	assert.Empty(requestsWith(log, "/issues/7"), "the thread is not fetched from the start again")
	assert.Equal([]string{
		"GET /repos/acme/widgets/pulls/7/reviews?limit=2&page=2",
		"GET /repos/acme/widgets/pulls/7/reviews/7004/comments",
	}, requestsWith(log, "/pulls/7/"))
	assert.Len(archived(t, st, source.ID), 5)
}

func TestDiscoveryResumesAnUnfinishedScan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	f.pageSize = 2
	f.limited["/notifications?all=true&limit=2&page=2"] = 3600
	source := newGiteaSource(t, st, f.forge())
	owners := forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}

	paused, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)
	assert.True(paused.Paused)
	assert.Equal(2, paused.Found, "the first page's threads are queued")
	pausedAt := len(f.requestLog())

	resumed, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Equal(4, resumed.Archived)
	assert.Equal([]string{
		"GET /notifications?all=true&limit=2&page=1",
		"GET /notifications?all=true&limit=2&page=2",
		"GET /notifications?all=true&limit=2&page=3",
	}, requestsWith(f.requestLog()[pausedAt:], "/notifications?all=true"),
		"the scan resumes a page early, and reads back to nothing since it never finished")
}

func TestReconcileChecksArchivedThreadsInRotation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	source := newGiteaSource(t, st, f.forge())
	owners := forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}
	_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)

	// ann replies on #4 after octo unwatched it: no notification, and no
	// activity of octo's. The reply is minutes old, so the fetch that reads
	// it reads it well after its update and holds it; a copy read within a
	// minute of an update is fetched once more, see
	// TestASameSecondChangeAfterAFetchIsFetchedAgain.
	replied := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	reply := comment(402, webBase+"acme/tools/issues/4", "Fixed now.", giteaUser(2002, "ann"), replied)
	f.comments["acme/tools#4"] = append(f.comments["acme/tools#4"], reply)
	f.issues["acme/tools#4"]["updated_at"] = reply["updated_at"]

	checks := func(log []string) []string {
		var threads []string
		for _, request := range requestsWith(log, "/issues/") {
			if !strings.Contains(request, "/comments") && !strings.Contains(request, "?") {
				threads = append(threads, strings.TrimPrefix(request, "GET /repos/"))
			}
		}
		return threads
	}
	sync := func() forge.Summary {
		before := len(f.requestLog())
		gitea := f.forge()
		gitea.reconcile = 2
		summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, owners)
		require.NoError(err)
		summary.Login = strings.Join(checks(f.requestLog()[before:]), ",")
		return summary
	}

	second := sync()
	assert.Equal("acme/widgets/issues/7,acme/widgets/issues/8", second.Login, "the two least recently fetched threads")
	assert.Zero(second.Found)

	third := sync()
	assert.Equal("acme/gadgets/issues/3,acme/tools/issues/4,acme/tools/issues/4", third.Login,
		"the rotation continues where it stopped, then fetches the changed thread")
	assert.Equal(1, third.Found, "the reply nothing else showed")
	assert.Equal(1, third.Added)
	assert.Contains(archived(t, st, source.ID), "comment:402")

	fourth := sync()
	assert.Equal("acme/tools/issues/4,acme/widgets/issues/7", fourth.Login,
		"the refetched thread comes after the rest of the rotation, which then starts over")
	assert.Zero(fourth.Found, "a reply is read back once")
}

func TestSyncDropsGoneThreadsAndRetriesFailingOnes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	ann := giteaUser(2002, "ann")
	f.issues["acme/widgets#10"] = issue(100, "acme", "widgets", 10, "Flaky", "Body.", ann, "2026-11-01T08:00:00Z", "2026-11-01T08:00:00Z")
	f.issues["acme/private#1"] = issue(110, "acme", "private", 1, "Private", "Body.", ann, "2026-11-01T08:00:00Z", "2026-11-01T08:00:00Z")
	f.fail["/repos/acme/widgets/issues/10/comments"] = 500
	f.refuse["/repos/acme/private/issues/1"] = "user does not have permission"
	f.notifications = []map[string]any{
		notified(1, "Issue", "acme", "widgets", 9, "Deleted", "2026-11-03T08:00:00Z"),
		notified(2, "Issue", "acme", "widgets", 10, "Flaky", "2026-11-02T08:00:00Z"),
		notified(3, "Issue", "acme", "private", 1, "Private", "2026-11-01T08:00:00Z"),
	}
	gitea := f.forge()
	source := newGiteaSource(t, st, gitea)

	summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{})
	require.ErrorIs(err, forge.ErrEveryThreadFailed, "no thread could be archived")
	assert.Equal(2, summary.Unavailable, "a deleted thread and a forbidden one are dropped")
	assert.Equal(1, summary.Failed)
	assert.Equal(1, summary.Remaining, "the failing thread waits for the next sync")
	assert.Len(requestsWith(f.requestLog(), "/issues/10/comments"), 4, "a server error is retried before the thread fails")
}

func TestForgejoThreadsKeepTheirMigratedAuthors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	frida, octo := forgejoUser(2002, "frida"), forgejoUser(1001, "octo")
	f.me = octo
	// Forgejo still names a migrated author in original_author beside the
	// ghost (FJ :28957 on Issue, :25560 on Comment), and its review comments
	// add extra_lines_count (FJ :30559).
	moved := issue(120, "acme", "imported", 1, "Imported", "From elsewhere.", ghost(), "2019-01-01T08:00:00Z", "2026-11-01T08:00:00Z")
	moved["original_author"], moved["original_author_id"] = "Old Name", 77
	f.issues["acme/imported#1"] = moved
	carried := comment(1201, webBase+"acme/imported/issues/1", "Carried.", ghost(), "2019-01-02T08:00:00Z")
	carried["original_author"], carried["original_author_id"] = "Other Name", 78
	f.comments["acme/imported#1"] = []map[string]any{carried, comment(1202, webBase+"acme/imported/issues/1", "Hello.", frida, "2026-11-01T08:00:00Z")}
	pr := pull(130, "imported", 2, "Port", "Ports it.", octo, "2026-11-01T08:00:00Z", "2026-11-01T09:00:00Z")
	f.issues["acme/imported#2"] = pr
	f.reviews["acme/imported#2"] = []map[string]any{review(1301, "REQUEST_CHANGES", "", frida, 1, "2026-11-01T08:30:00Z")}
	note := reviewComment(1302, 1301, "port.go", 4, 0, "Typo.", frida, nil, "2026-11-01T08:29:00Z")
	note["extra_lines_count"] = 0
	f.reviewComments[1301] = []map[string]any{note}
	f.notifications = []map[string]any{
		notified(1, "Issue", "acme", "imported", 1, "Imported", "2026-11-01T08:00:00Z"),
		notified(2, "Pull", "acme", "imported", 2, "Port", "2026-11-01T09:00:00Z"),
	}
	gitea := f.forge()
	source := newGiteaSource(t, st, gitea)

	summary, err := forge.NewSyncer(st, gitea, nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.Equal(6, summary.Added)
	messages := archived(t, st, source.ID)
	assert.NotEqual(Name, messages["issue:120"].SenderType, "a migrated author gets no identity")
	assert.NotEqual(Name, messages["comment:1201"].SenderType)
	assert.Equal(string(gitea.Instance())+":user:2002", messages["comment:1202"].SenderValue)
	assert.True(strings.HasPrefix(messages["review:1301"].Body, "Requested changes.\n\n"))
	assert.Contains(messages["comment:1302"].Raw, `"extra_lines_count":0`)
}

func TestAccountNamesTheMissingScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitea(t)
	f.refuse["/notifications"] = "token does not have at least one of required scope(s), required=[read:notification], token scope=read:user"

	_, err := f.forge().Account(t.Context())
	require.ErrorIs(err, ErrMissingScope)
	require.ErrorContains(err, "(read:notification)")

	_, err = f.forgeWith("another-token").Account(t.Context())
	assert.ErrorIs(err, forge.ErrUnauthorized)
}

func TestAuthorKinds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		user     *user
		original string
		want     forge.Account
	}{
		{"account", &user{ID: 7, Login: "ann", HTMLURL: webBase + "ann", Type: "User"},
			"", forge.Account{Kind: forge.AuthorAccount, ID: "7", Login: "ann", URL: webBase + "ann"}},
		{"bot", &user{ID: 8, Login: "renovate", Type: "Bot"},
			"", forge.Account{Kind: forge.AuthorAccount, ID: "8", Login: "renovate", Bot: true}},
		{"Gitea Actions", &user{ID: -2, Login: "gitea-actions", Type: "Bot"},
			"", forge.Account{Kind: forge.AuthorAccount, ID: "-2", Login: "gitea-actions", Bot: true}},
		{"Forgejo account, without a type", &user{ID: 9, Login: "frida"},
			"", forge.Account{Kind: forge.AuthorAccount, ID: "9", Login: "frida"}},
		{"deleted account", &user{ID: -1, Login: "Ghost", HTMLURL: webBase + "Ghost"},
			"", forge.Account{Kind: forge.AuthorUnavailable, Login: "Ghost", URL: webBase + "Ghost"}},
		{"no author", nil, "", forge.Account{Kind: forge.AuthorUnavailable, Login: "Ghost"}},
		{"Gitea's migrated author", &user{ID: -1, Login: "old-name"},
			"", forge.Account{Kind: forge.AuthorMigrated, Login: "old-name"}},
		{"Forgejo's migrated author", &user{ID: -1, Login: "Ghost"},
			"Old Name", forge.Account{Kind: forge.AuthorMigrated, Login: "Old Name"}},
	} {
		assert.Equal(t, tc.want, author(tc.user, tc.original), tc.name)
	}
}

func TestEvidenceKeepsOnlyWhatChangesWithThePost(t *testing.T) {
	got := evidence(jsontext.Value(`{"title":"T","labels":[{"name":"bug"}],"assets":[],"updated_at":"2026-11-08T08:00:00Z",
		"user":{"id":1,"login":"ann","email":"ann@noreply.git.example.com","last_login":"2026-11-08T07:00:00Z","followers_count":3},
		"milestone":{"id":2,"title":"v1","open_issues":4,"closed_issues":5},"body":"B"}`), issueActivity...)
	want, err := json.Marshal(map[string]any{
		"title": "T", "body": "B",
		"user":      map[string]any{"id": 1, "login": "ann"},
		"milestone": map[string]any{"id": 2, "title": "v1"},
	}, json.Deterministic(true))
	require.NoError(t, err)
	assert.Equal(t, want, got, "the same post is the same bytes")
}

func TestAReplyRewritesNoOtherPost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitea(t)
	seedThreads(f)
	source := newGiteaSource(t, st, f.forge())
	owners := forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}
	_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)

	// ann pushes and replies on #7: the pull request counts the reply and
	// moves its update time, each review goes stale, and both accounts have
	// signed in and gained followers since.
	now := time.Now().UTC().Format(time.RFC3339)
	pr := f.issues["acme/widgets#7"]
	ann, ok := pr["user"].(map[string]any)
	require.True(ok)
	octo, ok := f.comments["acme/widgets#7"][0]["user"].(map[string]any)
	require.True(ok)
	pr["updated_at"], pr["comments"], pr["pin_order"] = now, 2, 1
	for _, r := range f.reviews["acme/widgets#7"] {
		r["stale"], r["updated_at"] = true, now
	}
	for _, account := range []map[string]any{ann, octo} {
		account["last_login"], account["followers_count"] = now, 7
	}
	f.comments["acme/widgets#7"] = append(f.comments["acme/widgets#7"],
		comment(704, webBase+"acme/widgets/pulls/7", "Pushed a fix.", ann, now))
	f.notifications = append(f.notifications, notified(9, "Pull", "acme", "widgets", 7, "Add widgets", now))

	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, owners)
	require.NoError(err)
	assert.Equal(1, summary.Archived)
	assert.Equal(1, summary.Added, "the reply")
	assert.Zero(summary.Updated, "no other post changed")
}

func TestNewNamesTheServerAsParseInstanceDoes(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct{ raw, api string }{
		{"https://Codeberg.org/", "https://codeberg.org/api/v1"},
		{"http://nas.local:3000/git/", "http://nas.local:3000/git/api/v1"},
		{"https://git.example.com:443/forgejo", "https://git.example.com/forgejo/api/v1"},
	} {
		instance, _, err := forge.ParseInstance(tc.raw)
		require.NoError(t, err, tc.raw)
		base, err := url.Parse(tc.raw)
		require.NoError(t, err, tc.raw)
		gitea, err := New(base, "token", "test")
		require.NoError(t, err, tc.raw)
		assert.Equal(instance, gitea.Instance(), tc.raw)
		assert.Equal(tc.api, gitea.api.Base.String(), tc.raw)
	}
}
