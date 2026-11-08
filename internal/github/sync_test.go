package github

import (
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// account is the GitHub account the fake's token belongs to.
const accountID = "1001"

func newGitHubSource(t *testing.T, st *store.Store) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSourceContext(t.Context(), SourceType, accountID)
	require.NoError(t, err)
	// add-github confirms the account ID, so the account's posts are its own.
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), source.ID, accountID, "account-identifier"))
	return source
}

// seedThreads gives the fake one thread of every kind the account takes
// part in, plus threads it must leave alone.
func seedThreads(f *fakeGitHub) {
	octo, ann, bob := user("octo", 1001), user("ann", 2002), user("bob", 3003)
	releaseURL := f.server.URL + "/repos/acme/widgets/releases/55"
	f.notifications = []notification{
		notified("PullRequest", "acme", "widgets", "Add widgets", "https://api.github.com/repos/acme/widgets/pulls/7", "review_requested", "2026-11-07T09:00:00Z"),
		notified("Release", "acme", "widgets", "Widgets 1.0", releaseURL, "subscribed", "2026-11-06T08:00:00Z"),
		notified("Discussion", "acme", "widgets", "How do I configure widgets?", "", "mention", "2026-11-05T08:00:00Z"),
		notified("CheckSuite", "acme", "widgets", "CI failed", "", "ci_activity", "2026-11-04T08:00:00Z"),
		notified("Issue", "Secret-Corp", "plans", "Plans", "https://api.github.com/repos/Secret-Corp/plans/issues/1", "author", "2026-11-03T08:00:00Z"),
	}
	f.issueSearch = []searchNode{
		found("Issue", "acme", "gadgets", 3, "2026-10-01T08:00:00Z"),
		found("PullRequest", "acme", "widgets", 7, "2026-11-07T09:00:00Z"),
		found("Issue", "secret-corp", "plans", 2, "2026-10-02T08:00:00Z"),
	}

	approved := review{
		ID: "PRR_1", URL: "https://github.com/acme/widgets/pull/7#pullrequestreview-1", State: "APPROVED",
		SubmittedAt: new(ts("2026-11-07T08:30:00Z")), CreatedAt: ts("2026-11-07T08:29:00Z"), Author: octo,
		Comments: connection[reviewComment]{Nodes: []reviewComment{{
			ID: "PRRC_1", URL: "https://github.com/acme/widgets/pull/7#discussion_r1", Body: "Nit: rename.",
			Path: "widget.go", Line: new(12), CreatedAt: ts("2026-11-07T08:29:00Z"), Author: octo,
		}}},
	}
	f.threads["acme/widgets#7"] = issueThread{
		Typename: "PullRequest", ID: "PR_7", Number: 7, Title: "Add widgets",
		URL: "https://github.com/acme/widgets/pull/7", Body: "Adds widgets.", CreatedAt: ts("2026-11-06T10:00:00Z"), Author: ann,
		Comments: comments(issueComment{
			ID: "IC_1", URL: "https://github.com/acme/widgets/pull/7#issuecomment-1", Body: "Thanks!",
			CreatedAt: ts("2026-11-07T08:00:00Z"), Author: octo,
		}),
		Reviews: connection[review]{Nodes: []review{
			approved,
			{ID: "PRR_2", State: "PENDING", Body: "draft", CreatedAt: ts("2026-11-07T08:40:00Z"), Author: octo},
		}},
	}
	gadgets := issueThread{
		Typename: "Issue", ID: "I_3", Number: 3, Title: "Gadgets break",
		URL: "https://github.com/acme/gadgets/issues/3", CreatedAt: ts("2026-09-30T10:00:00Z"), Author: octo,
		Comments: comments(issueComment{
			ID: "IC_2", URL: "https://github.com/acme/gadgets/issues/3#issuecomment-2", Body: "Can you share a log?",
			CreatedAt: ts("2026-09-30T11:00:00Z"), Author: ann,
		}),
	}
	gadgets.Comments.PageInfo = pageInfo{HasNextPage: true, EndCursor: "c1"}
	f.threads["acme/gadgets#3"] = gadgets
	f.pages["MoreIssueComments I_3 c1"] = comments(issueComment{
		ID: "IC_3", URL: "https://github.com/acme/gadgets/issues/3#issuecomment-3", Body: "Fixed in main.",
		CreatedAt: ts("2026-10-01T08:00:00Z"), Author: ann,
	})
	f.threads["Secret-Corp/plans#1"] = issueThread{Typename: "Issue", ID: "I_S1", Number: 1, Title: "Plans", CreatedAt: ts("2026-11-03T07:00:00Z"), Author: ann}
	f.threads["secret-corp/plans#2"] = issueThread{Typename: "Issue", ID: "I_S2", Number: 2, Title: "Plans", CreatedAt: ts("2026-10-02T07:00:00Z"), Author: ann}

	answer := discussionComment{
		ID: "DC_1", URL: "https://github.com/acme/widgets/discussions/9#discussioncomment-1", Body: "In config.toml.",
		CreatedAt: ts("2026-11-05T07:00:00Z"), IsAnswer: true, Author: octo,
		Replies: connection[discussionReply]{Nodes: []discussionReply{{
			ID: "DC_2", URL: "https://github.com/acme/widgets/discussions/9#discussioncomment-2", Body: "Thanks!",
			CreatedAt: ts("2026-11-05T08:00:00Z"), ReplyTo: &nodeRef{ID: "DC_1"}, Author: bob,
		}}},
	}
	discussion := discussionThread{
		ID: "D_9", Number: 9, Title: "How do I configure widgets?", URL: "https://github.com/acme/widgets/discussions/9",
		Body: "Where is the config?", CreatedAt: ts("2026-11-05T06:00:00Z"), Author: bob,
		Comments: connection[discussionComment]{Nodes: []discussionComment{answer}},
	}
	discussion.Category.Name = "Q&A"
	f.discussions["acme/widgets#9"] = discussion

	f.releases["/repos/acme/widgets/releases/55"] = release{
		NodeID: "RE_55", TagName: "v1.0.0", Name: "Widgets 1.0", Body: "First release.",
		HTMLURL: "https://github.com/acme/widgets/releases/tag/v1.0.0", CreatedAt: ts("2026-11-06T07:00:00Z"),
		PublishedAt: new(ts("2026-11-06T08:00:00Z")), Author: &releaseAuthor{Login: "ann", ID: 2002, HTMLURL: "https://github.com/ann"},
	}
}

type archivedMessage struct {
	ID, Kind, Subject, Conversation string
	FromMe                          bool
	SenderType, SenderValue         string
	Body                            string
}

func archived(t *testing.T, st *store.Store, sourceID int64) map[string]archivedMessage {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT m.source_message_id, m.message_type, COALESCE(m.subject, ''), c.title, m.is_from_me,
		       pi.identifier_type, pi.identifier_value, COALESCE(mb.body_text, ''), m.metadata
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN participant_identifiers pi ON pi.participant_id = m.sender_id
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_id = ?
	`), sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]archivedMessage{}
	for rows.Next() {
		var (
			m           archivedMessage
			messageType string
			metadata    string
		)
		require.NoError(t, rows.Scan(&m.ID, &messageType, &m.Subject, &m.Conversation, &m.FromMe,
			&m.SenderType, &m.SenderValue, &m.Body, &metadata))
		assert.Equal(t, MessageType, messageType)
		var decoded struct {
			ProviderMetadata map[string]any `json:"provider_metadata"`
		}
		require.NoError(t, json.Unmarshal([]byte(metadata), &decoded))
		m.Kind, _ = decoded.ProviderMetadata["kind"].(string)
		out[m.ID] = m
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

	summary, err := NewSyncer(st, f.client(), nil).Sync(t.Context(), source.ID, Options{
		Owners: NewOwnerFilter(nil, []string{"secret-corp"}),
	})
	require.NoError(err)

	assert.Equal("octo", summary.Login)
	assert.Equal(4, summary.Archived, "pull request, issue, discussion and release")
	assert.Equal(11, summary.Added)
	assert.Equal(1, summary.Excluded, "the excluded owner's notification")
	assert.Equal(1, summary.Unsupported, "the CI notification")
	assert.Zero(summary.Remaining)

	for _, request := range f.requestLog() {
		if strings.Contains(strings.ToLower(request), "secret-corp") {
			assert.Contains(request, "-user:secret-corp", "an excluded owner appears only as a search exclusion")
		}
	}

	messages := archived(t, st, source.ID)
	require.Len(messages, 11)
	kinds := map[string]string{}
	for id, m := range messages {
		kinds[id] = m.Kind
	}
	assert.Equal(map[string]string{
		"PR_7": postPullRequest, "IC_1": postIssueComment, "PRR_1": postReview, "PRRC_1": postReviewComment,
		"I_3": postIssue, "IC_2": postIssueComment, "IC_3": postIssueComment,
		"D_9": postDiscussion, "DC_1": postDiscussionComment, "DC_2": postDiscussionReply,
		"RE_55": postRelease,
	}, kinds, "every post but the pending review")

	pr := messages["PR_7"]
	assert.Equal("Add widgets", pr.Subject)
	assert.Equal("[acme/widgets] Add widgets (PR #7)", pr.Conversation)
	assert.Equal("[acme/gadgets] Gadgets break (#3)", messages["IC_3"].Conversation)
	assert.Equal("[acme/widgets] How do I configure widgets? (Discussion #9)", messages["DC_2"].Conversation)
	assert.Equal("[acme/widgets] Release Widgets 1.0", messages["RE_55"].Conversation)
	assert.Contains(messages["PRR_1"].Body, "Approved these changes.")
	assert.Contains(messages["PRRC_1"].Body, "`widget.go` line 12")
	assert.Contains(messages["PRRC_1"].Body, "Nit: rename.")

	for _, id := range []string{"PR_7", "IC_2", "RE_55"} {
		assert.Equal(ParticipantIdentifierType, messages[id].SenderType)
		assert.Equal("2002", messages[id].SenderValue, "ann is one participant, keyed by account ID")
		assert.False(messages[id].FromMe)
	}
	for _, id := range []string{"IC_1", "PRR_1", "PRRC_1", "I_3", "DC_1"} {
		assert.True(messages[id].FromMe, "%s is the account's own post", id)
	}

	var conversationID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?
	`), source.ID, "PR_7").Scan(&conversationID))
	metadata, err := st.GetConversationMetadataContext(t.Context(), conversationID)
	require.NoError(err)
	assert.JSONEq(`{"github":{"kind":"pull_request","reason":"review_requested","url":"https://github.com/acme/widgets/pull/7"}}`, metadata.String)
}

func TestSyncOnlyRewritesWhatChanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)
	syncer := NewSyncer(st, f.client(), nil)
	opts := Options{Owners: NewOwnerFilter(nil, []string{"secret-corp"})}

	_, err := syncer.Sync(t.Context(), source.ID, opts)
	require.NoError(err)

	again, err := syncer.Sync(t.Context(), source.ID, opts)
	require.NoError(err)
	assert.Zero(again.Added)
	assert.Zero(again.Updated)
	assert.Contains(f.requestLog(), "GET /notifications?all=true&per_page=50&since=2026-11-07T09%3A00%3A00Z",
		"the second sync asks only for threads updated since the first")

	thread := f.threads["acme/gadgets#3"]
	thread.Title = "Gadgets break on Mondays"
	f.threads["acme/gadgets#3"] = thread
	f.issueSearch[0].UpdatedAt = ts("2026-11-08T08:00:00Z")

	edited, err := syncer.Sync(t.Context(), source.ID, opts)
	require.NoError(err)
	assert.Zero(edited.Added)
	assert.Equal(1, edited.Updated, "only the retitled issue's opening post changed")
	assert.Equal("[acme/gadgets] Gadgets break on Mondays (#3)", archived(t, st, source.ID)["IC_2"].Conversation)
}

func TestSyncLeavesTheRateLimitReserveAndResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)
	opts := Options{Owners: NewOwnerFilter(nil, []string{"secret-corp"})}
	// Discovery's three queries leave GraphQL at its 20% reserve.
	f.graphQLRemaining = 1001

	stopped, err := NewSyncer(st, f.client(), nil).Sync(t.Context(), source.ID, opts)
	require.NoError(err, "reaching the reserve ends the run without failing it")
	require.NotNil(stopped.RateLimit)
	assert.Equal("graphql", stopped.RateLimit.Resource)
	assert.Zero(stopped.Archived)
	assert.Equal(4, stopped.Remaining)
	assert.Empty(archived(t, st, source.ID))

	f.graphQLRemaining = 5000
	resumed, err := NewSyncer(st, f.client(), nil).Sync(t.Context(), source.ID, opts)
	require.NoError(err)
	assert.Nil(resumed.RateLimit)
	assert.Equal(4, resumed.Archived, "the queued threads, found before the limit")
	assert.Zero(resumed.Remaining)
	assert.Len(archived(t, st, source.ID), 11)
}

func TestSyncDropsThreadsItCannotRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	f.notifications = append(f.notifications,
		notified("Issue", "acme", "widgets", "Deleted", "https://api.github.com/repos/acme/widgets/issues/404", "author", "2026-11-08T08:00:00Z"))
	source := newGitHubSource(t, st)

	summary, err := NewSyncer(st, f.client(), nil).Sync(t.Context(), source.ID, Options{
		Owners: NewOwnerFilter(nil, []string{"secret-corp"}),
	})
	require.NoError(err)
	assert.Equal(1, summary.Unavailable)
	assert.Equal(4, summary.Archived)
	assert.Zero(summary.Remaining, "a thread that is gone is not retried")
}

func TestSyncFindsANewlyAllowedOwnersHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	seedThreads(f)
	source := newGitHubSource(t, st)
	syncer := NewSyncer(st, f.client(), nil)

	_, err := syncer.Sync(t.Context(), source.ID, Options{Owners: NewOwnerFilter(nil, []string{"secret-corp"})})
	require.NoError(err)
	summary, err := syncer.Sync(t.Context(), source.ID, Options{Owners: NewOwnerFilter(nil, nil)})
	require.NoError(err)

	assert.Equal(2, summary.Added, "both of the owner's issues, which earlier syncs never fetched")
	messages := archived(t, st, source.ID)
	assert.Contains(messages, "I_S1")
	assert.Contains(messages, "I_S2")
}

func TestSyncRejectsATokenOfAnotherAccount(t *testing.T) {
	st := testutil.NewTestStore(t)
	f := newFakeGitHub(t)
	f.viewer = Viewer{ID: 4004, Login: "someone-else"}
	source := newGitHubSource(t, st)

	_, err := NewSyncer(st, f.client(), nil).Sync(t.Context(), source.ID, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "someone-else")
}

func TestViewerRequiresAClassicTokenThatReadsNotifications(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scopes   string
		noScopes bool
		wantErr  error
	}{
		{name: "notifications scope", scopes: "notifications"},
		{name: "repo scope", scopes: "gist, repo"},
		{name: "other scopes only", scopes: "gist, read:org", wantErr: ErrClassicTokenRequired},
		{name: "fine-grained token", noScopes: true, wantErr: ErrClassicTokenRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.scopes, f.noScopes = tc.scopes, tc.noScopes
			viewer, err := f.client().Viewer(t.Context())
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, Viewer{ID: 1001, Login: "octo"}, viewer)
		})
	}
}

func TestSearchReadsALongHistoryInWindows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitHub(t)
	start := ts("2020-01-01T00:00:00Z")
	for i := range 1500 {
		f.issueSearch = append(f.issueSearch, searchNode{
			Typename: "Issue", Number: i + 1, UpdatedAt: start.Add(time.Duration(i) * time.Hour),
		})
		f.issueSearch[i].Repository.Name = "history"
		f.issueSearch[i].Repository.Owner.Login = "acme"
	}

	scan, err := f.client().searchInvolvement(t.Context(), searchIssues, "octo", time.Time{}, NewOwnerFilter(nil, nil))
	require.NoError(err)
	assert.Len(scan.Threads, 1500)
	assert.Equal(start.Add(1499*time.Hour), scan.Through)
	windows := slices.Compact(slices.Clone(f.searchQueries))
	require.Len(windows, 2, "one search per window of 1000 results")
	assert.NotContains(windows[0], "updated:>=")
	assert.Contains(windows[1], "updated:>=2020-02-11T15:00:00Z", "the second window starts at the first one's last update")
}

func TestMergeThreadsKeepsOneEntryPerThreadNewestFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	older := Thread{Kind: KindIssue, Owner: "acme", Repo: "widgets", Number: 1, UpdatedAt: ts("2026-11-01T00:00:00Z"), Reason: "mention"}
	newer := Thread{Kind: KindIssue, Owner: "ACME", Repo: "Widgets", Number: 1, UpdatedAt: ts("2026-11-03T00:00:00Z")}
	other := Thread{Kind: KindPullRequest, Owner: "acme", Repo: "widgets", Number: 2, UpdatedAt: ts("2026-11-02T00:00:00Z")}

	merged := mergeThreads([]Thread{older}, []Thread{other, newer})
	require.Len(merged, 2)
	assert.Equal(1, merged[0].Number)
	assert.Equal(ts("2026-11-03T00:00:00Z"), merged[0].UpdatedAt)
	assert.Equal("mention", merged[0].Reason, "a search hit keeps the notification's reason")
	assert.Equal(2, merged[1].Number)
}

func TestOwnerFilter(t *testing.T) {
	assert := assert.New(t)
	filter := NewOwnerFilter([]string{"Acme", "kenn-io"}, []string{" secret-corp "})
	assert.True(filter.Allows("ACME"))
	assert.False(filter.Allows("someone"), "only included owners")
	assert.Equal(" user:acme user:kenn-io -user:secret-corp", filter.searchQualifiers())

	exclude := NewOwnerFilter(nil, []string{"secret-corp"})
	assert.True(exclude.Allows("someone"))
	assert.False(exclude.Allows("Secret-Corp"))
}

func TestRateLimitedResponsesAreRateLimitErrors(t *testing.T) {
	f := newFakeGitHub(t)
	f.graphQLRemaining = 0
	err := f.client().graphQL(t.Context(), graphQLRequest{OperationName: "IssueThread", Variables: map[string]any{}}, &struct{}{})
	var limited *RateLimitError
	require.True(t, errors.As(err, &limited), "got %v", err)
	assert.Equal(t, f.graphQLReset.Unix(), limited.Reset.Unix())
}

func TestClientSendsTheTokenOnlyToTheAPIHost(t *testing.T) {
	f := newFakeGitHub(t)
	_, err := f.client().getJSON(t.Context(), "https://elsewhere.example/repos/acme/widgets/releases/55", nil, nil, &release{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside its API")
	assert.Empty(t, f.requestLog())
}
