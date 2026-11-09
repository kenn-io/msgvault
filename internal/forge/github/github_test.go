package github

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// accountID is the GitHub account the fake's token belongs to.
const accountID = "1001"

func newGitHubSource(t *testing.T, st *store.Store) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSourceContext(t.Context(), Name, forge.SourceIdentifier(Instance, accountID))
	require.NoError(t, err)
	// add-github confirms the account, so the account's posts are its own.
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), source.ID, forge.AccountKey(Instance, accountID), "account-identifier"))
	return source
}

var withoutSecretCorp = forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}

func syncGitHub(t *testing.T, st *store.Store, f *fakeGitHub, source *store.Source, opts forge.Options) (forge.Summary, error) {
	t.Helper()
	return forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, opts)
}

func comment(id, body, at string, author map[string]any) map[string]any {
	return map[string]any{"id": id, "url": "https://github.com/c/" + id, "body": body, "createdAt": at, "author": author}
}

// seedThreads gives the fake one thread of every kind the account takes
// part in, plus threads it must leave alone.
func seedThreads(f *fakeGitHub) {
	octo, ann, bob := user("octo", 1001), user("ann", 2002), user("bob", 3003)
	f.notifications = []notification{
		notified("PullRequest", "acme", "widgets", "Add widgets", "https://api.github.com/repos/acme/widgets/pulls/7", "review_requested", "2026-11-07T09:00:00Z"),
		notified("Release", "acme", "widgets", "Widgets 1.0", f.server.URL+"/repos/acme/widgets/releases/55", "subscribed", "2026-11-06T08:00:00Z"),
		notified("Discussion", "acme", "widgets", "How do I configure widgets?", "", "mention", "2026-11-05T08:00:00Z"),
		notified("CheckSuite", "acme", "widgets", "CI failed", "", "ci_activity", "2026-11-04T08:00:00Z"),
		notified("Issue", "Secret-Corp", "plans", "Plans", "https://api.github.com/repos/Secret-Corp/plans/issues/1", "author", "2026-11-03T08:00:00Z"),
	}
	f.issueSearch = []searchNode{
		found("Issue", "acme", "gadgets", 3, "2026-10-01T08:00:00Z"),
		found("PullRequest", "acme", "widgets", 7, "2026-11-07T09:00:00Z"),
		found("Issue", "secret-corp", "plans", 2, "2026-10-02T08:00:00Z"),
	}

	f.threads["acme/widgets#7"] = map[string]any{
		"__typename": "PullRequest", "id": "PR_7", "number": 7, "title": "Add widgets",
		"url": "https://github.com/acme/widgets/pull/7", "body": "Adds widgets.", "createdAt": "2026-11-06T10:00:00Z",
		"author":   ann,
		"comments": conn(comment("IC_1", "Thanks!", "2026-11-07T08:00:00Z", octo)),
		"reviews": conn(
			map[string]any{
				"id": "PRR_1", "url": "https://github.com/r/1", "body": "", "state": "APPROVED",
				"submittedAt": "2026-11-07T08:30:00Z", "createdAt": "2026-11-07T08:29:00Z", "author": octo,
				"comments": conn(map[string]any{
					"id": "PRRC_1", "url": "https://github.com/rc/1", "body": "Nit: rename.", "path": "widget.go",
					"line": 12, "originalLine": 12, "createdAt": "2026-11-07T08:29:00Z", "replyTo": nil, "author": octo,
				}),
			},
			map[string]any{
				"id": "PRR_2", "url": "", "body": "draft", "state": "PENDING", "createdAt": "2026-11-07T08:40:00Z",
				"author": octo, "comments": conn(),
			},
		),
	}
	f.threads["acme/gadgets#3"] = map[string]any{
		"__typename": "Issue", "id": "I_3", "number": 3, "title": "Gadgets break",
		"url": "https://github.com/acme/gadgets/issues/3", "body": "", "createdAt": "2026-09-30T10:00:00Z", "author": octo,
		"comments": connMore("c1", comment("IC_2", "Can you share a log?", "2026-09-30T11:00:00Z", ann)),
	}
	f.pages["MoreIssueComments I_3 c1"] = conn(comment("IC_3", "Fixed in main.", "2026-10-01T08:00:00Z", ann))
	f.threads["Secret-Corp/plans#1"] = map[string]any{"__typename": "Issue", "id": "I_S1", "number": 1, "title": "Plans",
		"url": "", "body": "", "createdAt": "2026-11-03T07:00:00Z", "author": ann, "comments": conn()}
	f.threads["secret-corp/plans#2"] = map[string]any{"__typename": "Issue", "id": "I_S2", "number": 2, "title": "Plans",
		"url": "", "body": "", "createdAt": "2026-10-02T07:00:00Z", "author": ann, "comments": conn()}

	f.discussions["acme/widgets#9"] = map[string]any{
		"id": "D_9", "number": 9, "title": "How do I configure widgets?", "url": "https://github.com/acme/widgets/discussions/9",
		"updatedAt": "2026-11-05T08:00:00Z",
		"body":      "Where is the config?", "createdAt": "2026-11-05T06:00:00Z", "category": map[string]any{"name": "Q&A"},
		"author": bob,
		"comments": conn(map[string]any{
			"id": "DC_1", "url": "https://github.com/dc/1", "body": "In config.toml.", "createdAt": "2026-11-05T07:00:00Z",
			"isAnswer": true, "author": octo,
			"replies": conn(map[string]any{
				"id": "DC_2", "url": "https://github.com/dc/2", "body": "Thanks!", "createdAt": "2026-11-05T08:00:00Z",
				"replyTo": map[string]any{"id": "DC_1"}, "author": bob,
			}),
		}),
	}

	f.releases["/repos/acme/widgets/releases/55"] = map[string]any{
		"node_id": "RE_55", "tag_name": "v1.0.0", "name": "Widgets 1.0", "body": "First release.",
		"html_url": "https://github.com/acme/widgets/releases/tag/v1.0.0", "created_at": "2026-11-06T07:00:00Z",
		"published_at": "2026-11-06T08:00:00Z", "prerelease": false,
		"author": map[string]any{"login": "ann", "id": 2002, "html_url": "https://github.com/ann", "type": "User"},
	}
}

type archivedMessage struct {
	Kind, Subject, Conversation string
	FromMe                      bool
	SenderType, SenderValue     string
	Body, RawFormat             string
}

func archived(t *testing.T, st *store.Store, sourceID int64) map[string]archivedMessage {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT m.source_message_id, m.message_type, COALESCE(m.subject, ''), c.title, m.is_from_me,
		       COALESCE((SELECT identifier_type FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), ''),
		       COALESCE((SELECT identifier_value FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), ''),
		       COALESCE(mb.body_text, ''), mr.raw_format, m.metadata
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN message_bodies mb ON mb.message_id = m.id
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE m.source_id = ?
	`), sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]archivedMessage{}
	for rows.Next() {
		var (
			id, messageType, metadata string
			m                         archivedMessage
		)
		require.NoError(t, rows.Scan(&id, &messageType, &m.Subject, &m.Conversation, &m.FromMe,
			&m.SenderType, &m.SenderValue, &m.Body, &m.RawFormat, &metadata))
		assert.Equal(t, Name, messageType)
		var decoded struct {
			ProviderMetadata map[string]any `json:"provider_metadata"`
		}
		require.NoError(t, json.Unmarshal([]byte(metadata), &decoded))
		m.Kind, _ = decoded.ProviderMetadata["kind"].(string)
		out[id] = m
	}
	require.NoError(t, rows.Err())
	return out
}

func TestSyncArchivesEveryThreadTheAccountTakesPartIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)

	summary, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Equal("octo", summary.Login)
	assert.Equal(4, summary.Archived, "pull request, issue, discussion and release")
	assert.Equal(11, summary.Added)
	assert.Zero(summary.Remaining)
	noUnexpectedRequests(t, f, "secret-corp")

	messages := archived(t, st, source.ID)
	kinds := map[string]string{}
	for id, m := range messages {
		kinds[id] = m.Kind
	}
	assert.Equal(map[string]string{
		"PR_7": postPullRequest, "IC_1": postIssueComment, "PRR_1": postReview, "PRRC_1": postReviewComment,
		"I_3": postIssue, "IC_2": postIssueComment, "IC_3": postIssueComment,
		"D_9": postDiscussion, "DC_1": postDiscussionComment, "DC_2": postDiscussionReply,
		"RE_55": postRelease,
	}, kinds, "every post but the pending review, the second comment page included")

	pr := messages["PR_7"]
	assert.Equal("Add widgets", pr.Subject)
	assert.Equal("[acme/widgets] Add widgets (PR #7)", pr.Conversation)
	assert.Equal(Name+"_json", pr.RawFormat)
	assert.Equal("[acme/gadgets] Gadgets break (#3)", messages["IC_3"].Conversation)
	assert.Equal("[acme/widgets] How do I configure widgets? (Discussion #9)", messages["DC_2"].Conversation)
	assert.Equal("[acme/widgets] Release Widgets 1.0", messages["RE_55"].Conversation)
	assert.Contains(messages["PRR_1"].Body, "Approved these changes.")
	assert.Contains(messages["PRRC_1"].Body, "`widget.go` line 12")

	for _, id := range []string{"PR_7", "IC_2", "RE_55"} {
		assert.Equal(Name, messages[id].SenderType)
		assert.Equal("github.com:user:2002", messages[id].SenderValue, "ann is one participant, keyed by instance and account ID")
		assert.False(messages[id].FromMe)
	}
	for _, id := range []string{"IC_1", "PRR_1", "PRRC_1", "I_3", "DC_1"} {
		assert.True(messages[id].FromMe, "%s is the account's own post", id)
	}
}

func TestSyncOnlyRewritesWhatChanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)

	_, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	before := len(f.requestLog())
	again, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Zero(again.Added)
	assert.Zero(again.Updated)
	second := f.requestLog()[before:]
	assert.Contains(second, "GET /notifications?all=true&per_page=50&since=2026-11-07T08%3A59%3A59Z",
		"the second sync asks for threads updated since the first, from a second before")
	for _, request := range second {
		assert.NotContains(request, "Thread ", "a thread the lists show again unchanged is not fetched again")
	}

	f.threads["acme/gadgets#3"]["title"] = "Gadgets break on Mondays"
	f.issueSearch[0].UpdatedAt = ts("2026-11-08T08:00:00Z")
	edited, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Zero(edited.Added)
	assert.Equal(1, edited.Updated, "only the retitled issue's opening post changed")
	assert.Equal("[acme/gadgets] Gadgets break on Mondays (#3)", archived(t, st, source.ID)["IC_2"].Conversation)
}

func TestSyncPausesAtTheRateLimitReserveAndResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)
	// Discovery's five queries (a discussion lookup, four searches) leave
	// GraphQL at its 20% reserve.
	f.graphQLRemaining = 1004

	paused, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err, "the reserve pauses the run without failing it")
	assert.True(paused.Paused)
	require.NotNil(paused.RateLimit)
	assert.Equal("graphql", paused.RateLimit.Resource)
	assert.Equal(4, paused.Remaining)

	f.graphQLRemaining = 5000
	resumed, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Equal(4, resumed.Archived, "the threads queued before the reserve")
	assert.Len(archived(t, st, source.ID), 11)
}

func TestASecondaryRateLimitPausesInsteadOfDroppingTheThread(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)
	f.refuse["IssueThread"] = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`

	summary, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.True(summary.Paused)
	assert.Zero(summary.Unavailable, "a throttled thread is not gone")
	assert.Positive(summary.Remaining)
}

func TestALongThreadResumesFromItsContinuation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	f.notifications = nil
	f.issueSearch = f.issueSearch[:1]
	source := newGitHubSource(t, st)
	f.refuse["MoreIssueComments"] = `{"message":"You have exceeded a secondary rate limit."}`

	paused, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.True(paused.Paused)
	assert.Equal(2, paused.Added, "the issue and its first comment page")

	delete(f.refuse, "MoreIssueComments")
	resumed, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Equal(1, resumed.Added, "the second comment page")
	assert.Equal(1, countPrefix(f.requestLog(), "IssueThread acme/gadgets#3"), "the thread's first page is not fetched again")
}

func countPrefix(log []string, prefix string) int {
	n := 0
	for _, entry := range log {
		if strings.HasPrefix(entry, prefix) {
			n++
		}
	}
	return n
}

func TestADiscussionNotificationFindsItsNumberAcrossListPages(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	f.discussions["acme/widgets#4"] = map[string]any{"id": "D_4", "number": 4, "title": "Another question", "updatedAt": "2026-11-06T08:00:00Z"}
	f.discussionPages = 1
	source := newGitHubSource(t, st)

	_, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Contains(t, archived(t, st, source.ID), "D_9", "the exact title on the list's second page")
}

func TestGraphQLErrorsKeepIncompleteThreadsQueued(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)
	// An author GitHub withholds leaves the post without one.
	f.gqlErrors["IssueThread acme/widgets#7"] = []map[string]any{{"type": "FORBIDDEN", "message": "Resource not accessible",
		"path": []any{"repository", "issueOrPullRequest", "comments", "nodes", 0, "author"}}}
	// A failed comment page makes the thread incomplete.
	f.gqlErrors["IssueThread acme/gadgets#3"] = []map[string]any{{"type": "SERVICE_UNAVAILABLE", "message": "Something went wrong",
		"path": []any{"repository", "issueOrPullRequest", "comments"}}}

	summary, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Equal(1, summary.Failed)
	assert.Equal(1, summary.Remaining, "the incomplete thread waits for the next sync")
	messages := archived(t, st, source.ID)
	assert.Contains(messages, "PR_7")
	assert.NotContains(messages, "I_3")
}

func TestAThreadWhoseRepositoryMovedToAnExcludedOwnerIsDropped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	f.repoOwners["acme/gadgets"] = "secret-corp"
	source := newGitHubSource(t, st)

	summary, err := syncGitHub(t, st, f, source, withoutSecretCorp)
	require.NoError(err)
	assert.Equal(1, summary.Excluded)
	assert.NotContains(f.requestLog(), "IssueThread acme/gadgets#3")
}

func TestSearchReadsALongHistoryInWindows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitHub(t)
	start := ts("2020-01-01T00:00:00Z")
	for i := range 1500 {
		f.issueSearch = append(f.issueSearch, found("Issue", "acme", "history", i+1, start.Add(time.Duration(i)*time.Hour).Format(time.RFC3339)))
	}

	var refs []forge.ThreadRef
	var through time.Time
	err := f.forge().searchThreads(t.Context(), searchIssues, "involves:octo", &cursor{}, &through, forge.NewOwnerFilter(nil, nil),
		func(page []forge.ThreadRef, _ []byte) error {
			refs = append(refs, page...)
			return nil
		})
	require.NoError(err)
	assert.Len(uniqueKeys(refs), 1500)
	assert.Equal(start.Add(1499*time.Hour), through)
	windows := slices.Compact(slices.Clone(f.searchQueries))
	require.Len(windows, 2, "one search per window of 1000 results")
	assert.Contains(windows[1], "updated:>=2020-02-11T15:00:00Z", "the second window starts at the first one's last update")
}

func TestSearchReadsBackForAnUpdateTheIndexShowsLate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitHub(t)
	f.issueSearch = []searchNode{found("Issue", "acme", "widgets", 2, "2026-11-07T10:03:00Z")}
	var through time.Time
	search := func() []string {
		var refs []forge.ThreadRef
		err := f.forge().searchThreads(t.Context(), searchIssues, "involves:octo", &cursor{}, &through, forge.NewOwnerFilter(nil, nil),
			func(page []forge.ThreadRef, _ []byte) error {
				refs = append(refs, page...)
				return nil
			})
		require.NoError(err)
		return uniqueKeys(refs)
	}
	assert.Equal([]string{"acme/widgets#2"}, search())

	// The index shows an update older than the one it showed already.
	f.issueSearch = append(f.issueSearch, found("Issue", "acme", "widgets", 1, "2026-11-07T10:00:00Z"))
	assert.Contains(search(), "acme/widgets#1", "the next search reads back past the latest update it read")
	assert.Equal(ts("2026-11-07T10:03:00Z"), through)
}

func TestSearchReadsASecondHoldingMoreThanAWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitHub(t)
	second := ts("2024-05-01T12:00:00Z")
	for i := range 1200 {
		f.issueSearch = append(f.issueSearch, found("Issue", "acme", "bulk", i+1, second.Format(time.RFC3339)))
		f.created[i+1] = ts("2010-01-01T00:00:00Z").Add(time.Duration(i) * 24 * time.Hour)
	}
	f.issueSearch = append(f.issueSearch, found("Issue", "acme", "bulk", 5000, "2024-05-02T00:00:00Z"))

	var refs []forge.ThreadRef
	var through time.Time
	err := f.forge().searchThreads(t.Context(), searchIssues, "involves:octo", &cursor{}, &through, forge.NewOwnerFilter(nil, nil),
		func(page []forge.ThreadRef, _ []byte) error {
			refs = append(refs, page...)
			return nil
		})
	require.NoError(err)
	assert.Len(uniqueKeys(refs), 1201, "every thread of the crowded second, then the later one")
	assert.Equal(ts("2024-05-02T00:00:00Z"), through)
}

func uniqueKeys(refs []forge.ThreadRef) []string {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, ref.Key)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func TestAccountRequiresAClassicTokenThatReadsNotifications(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scopes   string
		noScopes bool
		emails   bool
		wantErr  error
	}{
		{name: "notifications and public_repo scopes", scopes: "notifications, public_repo"},
		{name: "repo scope with read:user", scopes: "repo, read:user", emails: true},
		{name: "notifications scope alone, which can't read discussions", scopes: "notifications", wantErr: ErrClassicTokenRequired},
		{name: "public_repo scope alone, which can't read notifications", scopes: "public_repo", wantErr: ErrClassicTokenRequired},
		{name: "other scopes only", scopes: "gist, read:org", wantErr: ErrClassicTokenRequired},
		{name: "fine-grained token", noScopes: true, wantErr: ErrClassicTokenRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeGitHub(t)
			f.scopes, f.noScopes = tc.scopes, tc.noScopes
			adapter := f.forge()
			account, err := adapter.Account(t.Context())
			if tc.wantErr != nil {
				assert.ErrorIs(err, tc.wantErr)
				return
			}
			require.NoError(err)
			assert.Equal(forge.Account{Kind: forge.AuthorAccount, ID: accountID, Login: "octo", URL: "https://github.com/octo"}, account)
			assert.Equal(tc.emails, adapter.emails, "users' public emails are read only with a user scope")
		})
	}
}

func TestTheTokenGoesOnlyToTheAPIHost(t *testing.T) {
	f := newFakeGitHub(t)
	_, err := f.forge().client.getJSON(t.Context(), "https://elsewhere.example/repos/acme/widgets/releases/55", nil, nil, &struct{}{})
	require.ErrorContains(t, err, "outside its API")
	assert.Empty(t, f.requestLog())
}
