package gitlab

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
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

// The account the fake's token belongs to is userExample's.
const accountID = "1"

func newGitLabSource(t *testing.T, st *store.Store, instance forge.Instance) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSourceContext(t.Context(), Name, forge.SourceIdentifier(instance, accountID))
	require.NoError(t, err)
	// add-gitlab confirms the account, so the account's posts are its own.
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), source.ID, forge.AccountKey(instance, accountID), "account-identifier"))
	return source
}

// Projects and people of the seeded instance.
const (
	widgets = int64(5)  // acme/widgets
	gadgets = int64(6)  // acme/tools/gadgets, in a subgroup
	dots    = int64(7)  // john_smith/dotfiles, in the account's namespace
	plans   = int64(9)  // secret-corp/plans, an excluded owner's
	missing = int64(10) // a project the token may not read
)

type seeded struct {
	me, ann, bot, ghost, placeholder map[string]any
	// widgetsIssue is found by a to-do, gadgetsMR by an approval event and
	// the review list, dotsIssue by the authored list, importedIssue by a
	// comment event; secretIssue by all three, and must stay unread.
	widgetsIssue, gadgetsMR, dotsIssue, importedIssue, secretIssue *fakeThread
}

// seed gives the fake a thread found by each discovery stream, and an
// excluded owner's thread found by all of them.
func seed(t *testing.T, f *fakeGitLab) seeded {
	t.Helper()
	s := seeded{
		me:          person(t, 1, "john_smith", "John Smith"),
		ann:         person(t, 2, "ann", "Ann Example"),
		bot:         person(t, 3, "project_6_bot_1a2b", "CI bot"),
		ghost:       person(t, 4, "ghost", "Ghost User"),
		placeholder: person(t, 5, "placeholder_pat", "Pat Doe"),
	}
	f.userTypes[1] = userType(1, "HUMAN", false)
	f.userTypes[2] = userType(2, "HUMAN", false)
	f.userTypes[3] = userType(3, "PROJECT_BOT", true)
	f.userTypes[4] = userType(4, "GHOST", false)
	f.userTypes[5] = userType(5, "PLACEHOLDER", false)
	f.project(widgets, "acme/widgets")
	f.project(gadgets, "acme/tools/gadgets")
	f.project(dots, "john_smith/dotfiles")
	f.project(plans, "secret-corp/plans")

	system := fakeNote(t, 1130, nil, "added ~bug label", s.ann, "2026-11-05T09:30:00Z")
	system["system"] = true
	s.widgetsIssue = f.issue(widgets, 41, 1, "Widgets break", s.ann, "2026-11-05T10:00:00Z")
	s.widgetsIssue.discussions = []*discussionDoc{
		fakeDiscussion("d1", false,
			fakeNote(t, 1126, "DiscussionNote", "discussion text", s.me, "2026-11-05T09:00:00Z"),
			fakeNote(t, 1129, "DiscussionNote", "reply to the discussion", s.ghost, "2026-11-05T09:10:00Z"),
			system),
		fakeDiscussion("d2", true, fakeNote(t, 1128, nil, "a single comment", s.ann, "2026-11-05T09:20:00Z")),
	}

	s.gadgetsMR = f.mergeRequest(gadgets, 34, 1, "Add gadgets", s.ann, "2026-11-06T09:00:00Z", "reviews_for_me")
	s.gadgetsMR.discussions = []*discussionDoc{
		fakeDiscussion("m1", false,
			fakeNote(t, 2126, "DiscussionNote", "Please rename.", s.ann, "2026-11-06T08:00:00Z"),
			fakeNote(t, 2129, "DiscussionNote", "Done.", s.me, "2026-11-06T08:10:00Z")),
		fakeDiscussion("m2", true, fakeNote(t, 2128, nil, "Pipeline passed.", s.bot, "2026-11-06T08:20:00Z")),
		fakeDiscussion("m3", false, fakeDiffNote(t, 2130, "Use a constant here.", s.me)),
	}
	s.gadgetsMR.approvals = []map[string]any{approval(t, s.me, "2026-11-06T08:59:00Z")}

	s.dotsIssue = f.issue(dots, 42, 2, "Dotfiles cleanup", s.me, "2026-11-04T08:00:00Z", "created_by_me")
	s.dotsIssue.discussions = []*discussionDoc{fakeDiscussion("c1", true, fakeNote(t, 3001, nil, "Applied.", s.bot, "2026-11-04T07:00:00Z"))}

	s.importedIssue = f.issue(widgets, 44, 4, "Imported plan", s.placeholder, "2026-11-03T08:00:00Z")
	s.importedIssue.discussions = []*discussionDoc{fakeDiscussion("e1", true, fakeNote(t, 4001, nil, "Still relevant.", s.me, "2026-11-03T08:00:00Z"))}

	s.secretIssue = f.issue(plans, 43, 3, "Plans", s.ann, "2026-11-07T08:00:00Z", "assigned_to_me")
	s.secretIssue.discussions = []*discussionDoc{fakeDiscussion("x1", true, fakeNote(t, 9001, nil, "Confidential.", s.ann, "2026-11-07T08:00:00Z"))}

	f.todos = []map[string]any{
		fakeTodo(502, "mentioned", "2026-11-07T08:00:00Z", plans, "secret-corp/plans", s.secretIssue),
		fakeTodo(501, "mentioned", "2026-11-05T10:00:00Z", widgets, "acme/widgets", s.widgetsIssue),
	}
	f.commented(102, "2026-11-03T08:00:00.000Z", s.importedIssue)
	f.commented(103, "2026-11-07T08:00:00.000Z", s.secretIssue)
	f.event("approved", 101, "2026-11-06T08:59:00.000Z", s.gadgetsMR)
	return s
}

type archivedPost struct {
	Kind, Subject, Conversation, Body string
	FromMe                            bool
	SenderType, SenderValue           string
	Metadata                          map[string]any
}

func archived(t *testing.T, st *store.Store, sourceID int64) map[string]archivedPost {
	t.Helper()
	require := require.New(t)
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT m.source_message_id, COALESCE(m.subject, ''), c.title, m.is_from_me, m.metadata,
		       COALESCE((SELECT body_text FROM message_bodies mb WHERE mb.message_id = m.id), ''),
		       COALESCE((SELECT identifier_type FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), ''),
		       COALESCE((SELECT identifier_value FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?`), sourceID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	out := map[string]archivedPost{}
	for rows.Next() {
		var (
			id, metadata string
			p            archivedPost
		)
		require.NoError(rows.Scan(&id, &p.Subject, &p.Conversation, &p.FromMe, &metadata, &p.Body, &p.SenderType, &p.SenderValue))
		var decoded struct {
			ProviderMetadata map[string]any `json:"provider_metadata"`
		}
		require.NoError(json.Unmarshal([]byte(metadata), &decoded))
		p.Metadata = decoded.ProviderMetadata
		p.Kind, _ = p.Metadata["kind"].(string)
		out[id] = p
	}
	require.NoError(rows.Err())
	return out
}

func excludeSecretCorp() forge.Options {
	return forge.Options{Owners: forge.NewOwnerFilter(nil, []string{"secret-corp"})}
}

func TestSyncArchivesEveryThreadTheAccountTakesPartIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.discussionPage = 2
	seed(t, f)
	g := f.forge()
	source := newGitLabSource(t, st, g.Instance())

	summary, err := forge.NewSyncer(st, g, nil).Sync(t.Context(), source.ID, excludeSecretCorp())
	require.NoError(err)
	assert.Equal("john_smith", summary.Login)
	assert.Equal(4, summary.Found, "one thread from each stream; the excluded owner's never")
	assert.Equal(4, summary.Archived)
	assert.Equal(14, summary.Added)
	assert.Zero(summary.Remaining)

	log := f.requestLog()
	assert.Contains(log, "GET /api/v4/projects/6?", "an event names only the project, so its owner is looked up")
	assert.Empty(requestsFor(log, "/api/v4/projects/9/"), "nothing of the excluded owner's thread is fetched")
	assert.Len(requestsFor(log, "/api/v4/projects/6/merge_requests/1/discussions"), 2, "two pages of discussions")

	posts := archived(t, st, source.ID)
	kinds := map[string]string{}
	for id, p := range posts {
		kinds[id] = p.Kind
	}
	assert.Equal(map[string]string{
		"gid://gitlab/Issue/41": postIssue, "gid://gitlab/Note/1126": postDiscussionNote,
		"gid://gitlab/Note/1129": postDiscussionNote, "gid://gitlab/Note/1128": postNote,
		"gid://gitlab/MergeRequest/34": postMergeRequest, "gid://gitlab/Note/2126": postDiscussionNote,
		"gid://gitlab/Note/2129": postDiscussionNote, "gid://gitlab/Note/2128": postNote,
		"gid://gitlab/Note/2130": postDiffNote, "gid://gitlab/MergeRequest/34/approval/1": postApproval,
		"gid://gitlab/Issue/42": postIssue, "gid://gitlab/Note/3001": postNote,
		"gid://gitlab/Issue/44": postIssue, "gid://gitlab/Note/4001": postNote,
	}, kinds, "every post but the system note")

	issue := posts["gid://gitlab/Issue/41"]
	assert.Equal("Widgets break", issue.Subject)
	assert.Equal("[acme/widgets] Widgets break (#1)", posts["gid://gitlab/Note/1128"].Conversation)
	assert.Equal("[acme/tools/gadgets] Add gadgets (!1)", posts["gid://gitlab/MergeRequest/34/approval/1"].Conversation)
	assert.Equal("acme/widgets", issue.Metadata["repository"])
	assert.InDelta(1, issue.Metadata["number"], 0)

	reply := posts["gid://gitlab/Note/1129"]
	assert.Equal("d1", reply.Metadata["discussion"])
	assert.Equal("gid://gitlab/Note/1126", reply.Metadata["in_reply_to"])
	assert.Equal(false, reply.Metadata["resolved"])
	assert.NotContains(posts["gid://gitlab/Note/1126"].Metadata, "in_reply_to", "the first note starts the discussion")

	diff := posts["gid://gitlab/Note/2130"]
	assert.Equal("package.json", diff.Metadata["path"])
	assert.InDelta(27, diff.Metadata["line"], 0)
	assert.True(strings.HasPrefix(diff.Body, "`package.json` line 27"), diff.Body)
	assert.Contains(posts["gid://gitlab/MergeRequest/34/approval/1"].Body, "Approved")

	instance := string(g.Instance())
	assert.Equal(Name, posts["gid://gitlab/Note/1128"].SenderType)
	assert.Equal(instance+":user:2", posts["gid://gitlab/Note/1128"].SenderValue, "accounts are keyed by instance and user ID")
	assert.Equal(instance+":user:3", posts["gid://gitlab/Note/2128"].SenderValue, "a bot is an account")
	assert.NotEqual(Name, posts["gid://gitlab/Note/1129"].SenderType, "the ghost user gets no forge identity")
	assert.NotEqual(Name, posts["gid://gitlab/Issue/44"].SenderType, "an import placeholder gets no forge identity")
	for _, id := range []string{"gid://gitlab/Note/1126", "gid://gitlab/Note/2130", "gid://gitlab/MergeRequest/34/approval/1", "gid://gitlab/Issue/42"} {
		assert.True(posts[id].FromMe, "%s is the account's own post", id)
	}

	var conversationID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), source.ID, "gid://gitlab/Issue/41").Scan(&conversationID))
	metadata, err := st.GetConversationMetadataContext(t.Context(), conversationID)
	require.NoError(err)
	var facts struct {
		Forge struct {
			Ref forge.ThreadRef `json:"ref"`
		} `json:"forge"`
	}
	require.NoError(json.Unmarshal([]byte(metadata.String), &facts))
	assert.Equal("mentioned", facts.Forge.Ref.Reason, "the to-do's action")
	assert.Equal("projects/5/issues/1", facts.Forge.Ref.Locator)
}

func TestSyncDiscoversOnlyWhatChangedSinceTheLastSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	s := seed(t, f)
	source := newGitLabSource(t, st, f.forge().Instance())
	sync := func() forge.Summary {
		summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, excludeSecretCorp())
		require.NoError(err)
		return summary
	}
	first := sync()
	require.Equal(4, first.Archived)

	f.clearLog()
	again := sync()
	assert.Zero(again.Found, "every stream starts where the last sync stopped")
	assert.Zero(again.Added)
	assert.Zero(again.Updated)
	log := f.requestLog()
	assert.Equal([]string{"POST /api/graphql Todos after=<nil>"}, requestsFor(log, "/api/graphql Todos"),
		"one page of to-dos reaches the last scan's newest")
	commented := requestsFor(log, "/api/v4/users/1/events")[0]
	assert.Equal("2026-11-06", queryOf(t, commented).Get("after"), "events are asked for from the day before the newest one read")
	for _, list := range requestsFor(log, "/api/v4/merge_requests") {
		if queryOf(t, list).Get("scope") == "reviews_for_me" {
			assert.Equal("2026-11-06T09:00:00.000Z", queryOf(t, list).Get("updated_after"), "from the newest update read")
		}
	}
	checks := requestsFor(log, "/api/v4/projects/5/issues?")
	require.Len(checks, 1, "archived threads are checked a project at a time")
	assert.ElementsMatch([]string{"1", "4"}, queryOf(t, checks[0])["iids[]"])
	assert.NotEmpty(queryOf(t, checks[0]).Get("updated_after"), "since the last sync started")

	// A mention in the authored issue, a new reply on the merge request
	// under review, and a reply in the to-do's thread that nothing but the
	// check of archived threads finds.
	s.dotsIssue.object["updated_at"] = "2026-11-08T10:00:00.000Z"
	s.dotsIssue.discussions = append(s.dotsIssue.discussions,
		fakeDiscussion("c2", true, fakeNote(t, 3002, nil, "@john_smith can you look?", s.ann, "2026-11-08T10:00:00Z")))
	f.todos = append([]map[string]any{fakeTodo(503, "directly_addressed", "2026-11-08T10:00:00Z", dots, "john_smith/dotfiles", s.dotsIssue)}, f.todos...)
	s.gadgetsMR.object["updated_at"] = "2026-11-08T11:00:00.000Z"
	s.gadgetsMR.discussions = append(s.gadgetsMR.discussions,
		fakeDiscussion("m4", true, fakeNote(t, 2140, nil, "Merging now.", s.ann, "2026-11-08T11:00:00Z")))
	s.widgetsIssue.object["updated_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	d1 := s.widgetsIssue.discussions[0]
	d1.Notes = append(slices.Clone(d1.Notes), fakeNote(t, 1131, "DiscussionNote", "Fixed upstream.", s.ann, "2026-11-08T12:00:00Z"))

	changed := sync()
	assert.Equal(3, changed.Archived, "the three changed threads")
	assert.Equal(3, changed.Added, "their new posts")
	assert.Zero(changed.Updated, "a reply leaves the opening post's evidence as it was")
	posts := archived(t, st, source.ID)
	assert.Contains(posts, "gid://gitlab/Note/3002")
	assert.Contains(posts, "gid://gitlab/Note/2140")
	assert.Equal("gid://gitlab/Note/1126", posts["gid://gitlab/Note/1131"].Metadata["in_reply_to"])
}

func TestSyncResumesAThreadWhereARateLimitStoppedIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.project(gadgets, "acme/tools/gadgets")
	me, ann := person(t, 1, "john_smith", "John Smith"), person(t, 2, "ann", "Ann Example")
	mr := f.mergeRequest(gadgets, 34, 1, "Add gadgets", me, "2026-11-06T09:00:00Z", "created_by_me")
	for i, body := range []string{"one", "two", "three", "four", "five"} {
		id := int64(2001 + i)
		mr.discussions = append(mr.discussions, fakeDiscussion("m"+body, true, fakeNote(t, id, nil, body, ann, "2026-11-06T08:00:00Z")))
	}
	mr.approvals = []map[string]any{approval(t, ann, "2026-11-06T08:59:00Z")}
	f.discussionPage = 2
	f.limitPath, f.limitAfter = "/api/v4/projects/6/merge_requests/1/discussions", 1
	source := newGitLabSource(t, st, f.forge().Instance())

	paused, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err, "a rate limit pauses the run without failing it")
	assert.True(paused.Paused)
	require.NotNil(paused.RateLimit)
	assert.Equal(3, paused.Added, "the merge request and the first page of discussions")
	assert.Equal(1, paused.Remaining)

	f.limitPath = ""
	f.clearLog()
	resumed, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Equal(1, resumed.Archived)
	assert.Equal(4, resumed.Added, "three more discussions and the approval")
	log := f.requestLog()
	assert.Empty(requestsFor(log, "/api/v4/projects/6/merge_requests/1?"), "the merge request is not fetched again")
	discussions := requestsFor(log, "/api/v4/projects/6/merge_requests/1/discussions")
	require.NotEmpty(discussions)
	assert.Equal("2", queryOf(t, discussions[0]).Get("page"), "the fetch continues at the page the limit stopped")
	assert.Len(archived(t, st, source.ID), 7)
}

func TestSyncDropsGoneThreadsAndRetriesFailingOnes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	seed(t, f)
	gone := &fakeThread{resource: resourceIssues, object: map[string]any{"id": 99, "iid": 99, "updated_at": "2026-11-08T08:00:00Z"}}
	f.todos = append([]map[string]any{fakeTodo(504, "assigned", "2026-11-08T08:00:00Z", widgets, "acme/widgets", gone)}, f.todos...)
	f.failPath = "/api/v4/projects/7/issues/2/discussions"
	source := newGitLabSource(t, st, f.forge().Instance())

	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, excludeSecretCorp())
	require.NoError(err)
	assert.Equal(1, summary.Unavailable, "the deleted issue")
	assert.Equal(1, summary.Failed, "the issue whose discussions fail")
	assert.Equal(3, summary.Archived)
	assert.Equal(1, summary.Remaining, "the failed thread waits for the next sync")

	f.failPath = ""
	retried, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, excludeSecretCorp())
	require.NoError(err)
	assert.Equal(1, retried.Archived)
	assert.Contains(archived(t, st, source.ID), "gid://gitlab/Note/3001")
}

func TestSyncRejectsATokenOfAnotherAccount(t *testing.T) {
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.me = fixture(t, userExample, map[string]any{"id": 77, "username": "someone_else"})
	source := newGitLabSource(t, st, f.forge().Instance())

	_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.ErrorContains(t, err, "someone_else")
}

func TestAccountChecksTheTokensScopes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scopes   []string
		noScopes bool
		missing  string
	}{
		{name: "read_api and read_user", scopes: []string{"read_api", "read_user"}},
		{name: "api", scopes: []string{"api"}},
		{name: "without read_user", scopes: []string{"read_api", "read_repository"}, missing: "read_user"},
		{name: "without either", scopes: []string{"read_repository"}, missing: "read_api and read_user"},
		{name: "a token GitLab doesn't describe", noScopes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			f := newFakeGitLab(t)
			f.scopes, f.noScopes = tc.scopes, tc.noScopes
			account, err := f.forge().Account(t.Context())
			if tc.missing != "" {
				require.ErrorIs(t, err, ErrMissingScope)
				assert.ErrorContains(err, tc.missing)
				return
			}
			require.NoError(t, err)
			assert.Equal(forge.Account{
				Kind: forge.AuthorAccount, ID: "1", Login: "john_smith", URL: "http://localhost:3000/john_smith",
			}, account, "never the account's email")
		})
	}
}

func TestOwnerIsTheProjectsCurrentTopLevelNamespace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitLab(t)
	f.project(widgets, "new-home/team/widgets")
	ref := forge.ThreadRef{Kind: forge.KindIssue, Key: "gid://gitlab/Issue/41", Owner: "acme", Repo: "acme/widgets", Number: 1, Locator: "projects/5/issues/1"}

	g := f.forge()
	owner, err := g.Owner(t.Context(), ref)
	require.NoError(err)
	assert.Equal("new-home", owner, "the project moved since it was found")
	_, err = g.Owner(t.Context(), ref)
	require.NoError(err)
	assert.Len(requestsFor(f.requestLog(), "/api/v4/projects/5"), 1, "asked once per sync")

	_, err = f.forge().Owner(t.Context(), ref)
	require.NoError(err)
	assert.Len(requestsFor(f.requestLog(), "/api/v4/projects/5"), 2, "and again by the next sync")

	_, err = g.Owner(t.Context(), forge.ThreadRef{Key: "gid://gitlab/Issue/1", Locator: "projects/10/issues/1"})
	assert.ErrorIs(err, forge.ErrUnavailable, "a project the token may not read")
}

func TestToDoWatermarkMovesOnlyAfterACompleteScan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitLab(t)
	s := seed(t, f)
	f.todoPage = 1
	stop := errors.New("stop")

	var cursors []cursor
	err := f.forge().Discover(t.Context(), nil, forge.NewOwnerFilter(nil, nil), noKnownThreads{},
		func(_ []forge.ThreadRef, next []byte) error {
			var c cursor
			require.NoError(json.Unmarshal(next, &c))
			cursors = append(cursors, c)
			return stop
		})
	require.ErrorIs(err, stop)
	require.Len(cursors, 1)
	assert.True(cursors[0].Todos.Through.IsZero(), "the first page doesn't move the watermark")
	require.NotNil(cursors[0].Todos.Scan)
	assert.Equal("1", cursors[0].Todos.Scan.After)

	f.clearLog()
	f.todos = append([]map[string]any{fakeTodo(505, "mentioned", "2026-11-08T09:00:00Z", dots, "john_smith/dotfiles", s.dotsIssue)}, f.todos...)
	resumed := cursors[0].encode()
	var refs []forge.ThreadRef
	var last cursor
	require.NoError(f.forge().Discover(t.Context(), resumed, forge.NewOwnerFilter(nil, nil), noKnownThreads{},
		func(found []forge.ThreadRef, next []byte) error {
			refs = append(refs, found...)
			last = cursor{}
			return json.Unmarshal(next, &last)
		}))
	todos := requestsFor(f.requestLog(), "/api/graphql Todos")
	require.NotEmpty(todos)
	assert.Equal("POST /api/graphql Todos after=1", todos[0], "the scan continues where it stopped")
	assert.Nil(last.Todos.Scan)
	assert.Equal(time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC), last.Todos.Through.UTC(),
		"the watermark is the newest to-do the interrupted scan read; a newer one waits for the next scan")
	assert.Equal([]string{"gid://gitlab/Todo/502"}, last.Todos.Seen)
}

func TestForbiddenIsUnavailableUnlessTheTokenLacksAScope(t *testing.T) {
	require := require.New(t)
	forbidden := &forge.HTTPError{Status: http.StatusForbidden, Request: "GET /api/v4/projects/5", Message: "403 Forbidden"}
	require.ErrorIs(forbiddenIsUnavailable(forbidden), forge.ErrUnavailable)
	scope := &forge.HTTPError{Status: http.StatusForbidden, Request: "GET /api/v4/events",
		Message: `{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token.","scope":"api read_api"}`}
	require.NotErrorIs(forbiddenIsUnavailable(scope), forge.ErrUnavailable)
	require.NotErrorIs(forbiddenIsUnavailable(&forge.HTTPError{Status: http.StatusBadRequest}), forge.ErrUnavailable)
}

func TestLimitedReadsGitLabsRateLimitHeaders(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	c := &client{now: func() time.Time { return now }}
	limited := func(status int, header http.Header) (time.Time, bool) {
		return c.limited(&http.Response{StatusCode: status, Header: header})
	}

	_, ok := limited(http.StatusOK, http.Header{"Ratelimit-Remaining": {"0"}})
	assert.False(ok, "only a 429 is a refusal")
	reset, ok := limited(http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}})
	assert.True(ok)
	assert.Equal(now.Add(30*time.Second), reset)
	reset, _ = limited(http.StatusTooManyRequests, http.Header{"Ratelimit-Resettime": {"Sun, 08 Nov 2026 12:05:00 GMT"}})
	assert.Equal(now.Add(5*time.Minute), reset.UTC())
	reset, _ = limited(http.StatusTooManyRequests, http.Header{"Ratelimit-Reset": {"1794142920"}})
	assert.Equal(time.Unix(1794142920, 0), reset)
	reset, _ = limited(http.StatusTooManyRequests, http.Header{})
	assert.Equal(now.Add(unknownWait), reset)
}

func TestPaceWaitsOutASpentLimitOrPausesForALongOne(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	c := &client{now: func() time.Time { return now }}
	c.observe(&http.Response{Header: http.Header{"Ratelimit-Remaining": {"0"}, "Ratelimit-Reset": {"0"}}})
	require.NoError(t, c.pace(t.Context()), "a reset in the past")

	c.limit = rateLimit{remaining: 0, reset: now.Add(time.Hour)}
	err := c.pace(t.Context())
	var limited *forge.RateLimitError
	require.ErrorAs(t, err, &limited)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.limit = rateLimit{remaining: 0, reset: now.Add(30 * time.Second)}
	assert.ErrorIs(c.pace(ctx), context.Canceled, "a short reset is waited out, until the sync stops")
}

func TestReferencesAndGlobalIDs(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("group/sub/project", referencePath("group/sub/project#6"))
	assert.Equal("group/project", referencePath("group/project!7"))
	assert.Empty(referencePath("#6"))
	n, ok := gidNumber("gid://gitlab/Project/5")
	assert.True(ok)
	assert.Equal(int64(5), n)
	_, ok = gidNumber("gid://other/Project/5")
	assert.False(ok)
	assert.Equal("acme", ownerOf("acme/tools/gadgets"))

	ref := threadRef(resourceMergeRequests, 34, 6, 1, "acme/tools/gadgets", time.Time{})
	assert.Equal("gid://gitlab/MergeRequest/34", ref.Key)
	assert.Equal(forge.KindMergeRequest, ref.Kind)
	loc, err := parseLocator(ref)
	require.NoError(t, err)
	assert.Equal(locator{project: 6, resource: resourceMergeRequests, iid: 1}, loc)
}

func TestNewDefaultsToGitLabCom(t *testing.T) {
	assert := assert.New(t)
	g, err := New("", "token", "msgvault-test")
	require.NoError(t, err)
	assert.Equal(forge.Instance("gitlab.com"), g.Instance())
	assert.Equal("https://gitlab.com/api/v4", g.client.rest.Base.String())
	assert.Equal("https://gitlab.com/api", g.client.graph.Base.String())

	g, err = New("https://git.example.com:8443/gitlab/", "token", "msgvault-test")
	require.NoError(t, err)
	assert.Equal(forge.Instance("git.example.com:8443/gitlab"), g.Instance())
	assert.Equal("https://git.example.com:8443/gitlab/api/v4", g.client.rest.Base.String(), "the API sits under the instance's path")
}

// noKnownThreads is a source with nothing archived yet.
type noKnownThreads struct{}

func (noKnownThreads) Oldest(context.Context, int) ([]forge.KnownThread, error)  { return nil, nil }
func (noKnownThreads) Each(context.Context, func(forge.KnownThread) error) error { return nil }
