package forge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	fakeName     = "fakeforge"
	fakeInstance = Instance("forge.example.com:8443/git")
)

// fakeForge is a forge whose discovery pages and thread posts tests choose.
// Cursors and continuations are page indexes.
type fakeForge struct {
	account Account
	// pages are discovery pages; Discover resumes at the cursor's index.
	pages [][]ThreadRef
	// threads holds each thread's pages of posts, by key.
	threads map[string][][]Post
	// owners overrides a thread's current owner, by key.
	owners map[string]string
	// failures makes Fetch fail at a page, by key.
	failures map[string]fetchFailure
	// afterPage runs after Discover reports a page.
	afterPage func(index int)
	// afterPost runs after the core took a thread's page of posts.
	afterPost func(key string, index int)
	// titles replaces a thread's title, by key.
	titles map[string]string
	// discover, when set, replaces the paged discovery.
	discover func(ctx context.Context, known KnownThreads, page func([]ThreadRef, []byte) error) error

	cursors []string
	fetches []string
}

type fetchFailure struct {
	page int
	err  error
}

func newFakeForge() *fakeForge {
	return &fakeForge{
		account:  Account{Kind: AuthorAccount, ID: "1001", Login: "octo"},
		threads:  map[string][][]Post{},
		owners:   map[string]string{},
		failures: map[string]fetchFailure{},
	}
}

func (f *fakeForge) Name() string       { return fakeName }
func (f *fakeForge) Instance() Instance { return fakeInstance }

func (f *fakeForge) Account(context.Context) (Account, error) { return f.account, nil }

func (f *fakeForge) Discover(ctx context.Context, cursor []byte, _ OwnerFilter, known KnownThreads,
	page func([]ThreadRef, []byte) error,
) error {
	f.cursors = append(f.cursors, string(cursor))
	if f.discover != nil {
		return f.discover(ctx, known, page)
	}
	start := index(cursor)
	for i := start; i < len(f.pages); i++ {
		if err := page(f.pages[i], []byte(strconv.Itoa(i+1))); err != nil {
			return err
		}
		if f.afterPage != nil {
			f.afterPage(i)
		}
	}
	return nil
}

func (f *fakeForge) Owner(_ context.Context, ref ThreadRef) (string, error) {
	if owner, ok := f.owners[ref.Key]; ok {
		return owner, nil
	}
	return ref.Owner, nil
}

func (f *fakeForge) Fetch(_ context.Context, ref ThreadRef, continuation []byte,
	page func(ThreadHead, []Post, []byte) error,
) error {
	f.fetches = append(f.fetches, ref.Key+"@"+string(continuation))
	pages, ok := f.threads[ref.Key]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnavailable, ref.Key)
	}
	head := ThreadHead{ConversationID: "conv-" + ref.Key, Title: "[" + ref.Repo + "] Thread " + ref.Key, URL: "https://forge.example.com/" + ref.Key}
	if title, ok := f.titles[ref.Key]; ok {
		head.Title = title
	}
	for i := index(continuation); i < len(pages); i++ {
		if failure, ok := f.failures[ref.Key]; ok && failure.page == i {
			return failure.err
		}
		var next []byte
		if i+1 < len(pages) {
			next = []byte(strconv.Itoa(i + 1))
		}
		if err := page(head, pages[i], next); err != nil {
			return err
		}
		if f.afterPost != nil {
			f.afterPost(ref.Key, i)
		}
	}
	return nil
}

func index(cursor []byte) int {
	if len(cursor) == 0 {
		return 0
	}
	n, err := strconv.Atoi(string(cursor))
	if err != nil {
		panic(err)
	}
	return n
}

func ref(key, owner string, updated string) ThreadRef {
	at, err := time.Parse(time.RFC3339, updated)
	if err != nil {
		panic(err)
	}
	return ThreadRef{Kind: KindIssue, Key: key, Owner: owner, Repo: owner + "/repo", Number: len(key), UpdatedAt: at}
}

func post(id, kind string, author Account, title, body, at string) Post {
	created, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic(err)
	}
	return Post{
		ID: id, Kind: kind, Author: author, Title: title, Body: body,
		URL: "https://forge.example.com/" + id, CreatedAt: created,
		Metadata: map[string]any{"repository": "acme/repo"},
		Raw:      []byte(`{"id":"` + id + `","body":"` + body + `"}`),
	}
}

var (
	octo  = Account{Kind: AuthorAccount, ID: "1001", Login: "octo"}
	ann   = Account{Kind: AuthorAccount, ID: "2002", Login: "ann", Email: "ann@example.com"}
	robot = Account{Kind: AuthorAccount, ID: "3003", Login: "ci-bot", Email: "bot@example.com", Bot: true}
	ghost = Account{Kind: AuthorUnavailable, Login: "ghost", URL: "https://forge.example.com/ghost"}
	moved = Account{Kind: AuthorMigrated, Login: "Old Name"}
)

func newForgeSource(t *testing.T, st *store.Store) *store.Source {
	t.Helper()
	source, err := st.GetOrCreateSourceContext(t.Context(), fakeName, SourceIdentifier(fakeInstance, "1001"))
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), source.ID, AccountKey(fakeInstance, "1001"), "account-identifier"))
	return source
}

type runRow struct {
	Status          string
	Processed, Adds int64
}

func runs(t *testing.T, st *store.Store, sourceID int64) []runRow {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(
		`SELECT status, messages_processed, messages_added FROM sync_runs WHERE source_id = ? ORDER BY id`), sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []runRow
	for rows.Next() {
		var row runRow
		require.NoError(t, rows.Scan(&row.Status, &row.Processed, &row.Adds))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func resumeState(t *testing.T, st *store.Store, sourceID int64) syncState {
	t.Helper()
	var blob string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COALESCE(cursor_before, '') FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), sourceID).Scan(&blob))
	state, err := decodeState(blob)
	require.NoError(t, err)
	return state
}

func TestSyncArchivesEveryPostOfDiscoveredThreads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{
		{ref("i1", "acme", "2026-11-07T10:00:00Z"), ref("x1", "secret", "2026-11-07T11:00:00Z")},
		{ref("i2", "acme", "2026-11-06T10:00:00Z")},
	}
	f.threads["i1"] = [][]Post{
		{post("p1", "issue", ann, "Widgets break", "They break.", "2026-11-01T10:00:00Z")},
		{post("c1", "issue_comment", octo, "", "Fixed.", "2026-11-02T10:00:00Z"),
			post("c2", "issue_comment", robot, "", "CI passed.", "2026-11-02T11:00:00Z")},
	}
	f.threads["i2"] = [][]Post{
		{post("p2", "issue", ghost, "Old", "From a deleted account.", "2020-01-01T10:00:00Z"),
			post("c3", "issue_comment", moved, "", "Carried over.", "2020-01-02T10:00:00Z")},
	}
	f.threads["x1"] = [][]Post{{post("px", "issue", ann, "Secret", "Must not be archived.", "2026-11-07T10:00:00Z")}}

	summary, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{Owners: NewOwnerFilter(nil, []string{"secret"})})
	require.NoError(err)
	assert.Equal("octo", summary.Login)
	assert.Equal(2, summary.Found, "the excluded owner's thread is never queued")
	assert.Equal(2, summary.Archived)
	assert.Equal(5, summary.Added)
	assert.Zero(summary.Remaining)
	assert.False(summary.Paused)
	assert.NotContains(f.fetches, "x1@", "an excluded thread is never fetched")
	assert.Equal([]runRow{{Status: "completed", Processed: 5, Adds: 5}}, runs(t, st, source.ID))

	type message struct {
		Subject, Conversation, ConversationType, RawFormat string
		FromMe                                             bool
		SenderType, SenderValue                            string
	}
	got := map[string]message{}
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT m.source_message_id, COALESCE(m.subject, ''), c.title, c.conversation_type, mr.raw_format, m.is_from_me,
		       COALESCE((SELECT identifier_type FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), ''),
		       COALESCE((SELECT identifier_value FROM participant_identifiers pi WHERE pi.participant_id = m.sender_id ORDER BY pi.is_primary DESC LIMIT 1), '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE m.source_id = ?`), source.ID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var m message
		require.NoError(rows.Scan(&id, &m.Subject, &m.Conversation, &m.ConversationType, &m.RawFormat, &m.FromMe, &m.SenderType, &m.SenderValue))
		got[id] = m
	}
	require.NoError(rows.Err())
	require.Len(got, 5)

	assert.Equal("Widgets break", got["p1"].Subject)
	assert.Equal("[acme/repo] Thread i1", got["c2"].Conversation, "the opening post names the conversation")
	assert.Equal(ConversationType, got["c1"].ConversationType)
	assert.Equal(fakeName+"_json", got["c1"].RawFormat)
	assert.True(got["c1"].FromMe, "the account's own post")
	assert.False(got["p1"].FromMe)
	assert.Equal(fakeName, got["c1"].SenderType)
	assert.Equal("forge.example.com:8443/git:user:1001", got["c1"].SenderValue, "accounts are keyed by instance and ID")
	assert.Equal("forge.example.com:8443/git:user:3003", got["c2"].SenderValue, "a bot never links by email")
	assert.NotEqual(fakeName, got["p2"].SenderType, "a deleted account gets no forge identity")
	assert.NotEqual(fakeName, got["c3"].SenderType, "a migrated name gets no forge identity")

	known := &knownThreads{store: st, sourceID: source.ID}
	var keys []string
	require.NoError(known.Each(t.Context(), func(k KnownThread) error {
		keys = append(keys, k.Ref.Key)
		assert.False(k.CheckedAt.IsZero())
		return nil
	}))
	assert.ElementsMatch([]string{"i1", "i2"}, keys)
}

func TestSyncResumesAThreadWhereARateLimitPausedIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{
		{post("p1", "issue", ann, "Title", "Body.", "2026-11-01T10:00:00Z")},
		{post("c1", "issue_comment", octo, "", "Reply.", "2026-11-02T10:00:00Z")},
	}
	reset := time.Now().Add(time.Hour)
	f.failures["i1"] = fetchFailure{page: 1, err: &RateLimitError{Resource: "graphql", Reset: reset}}

	paused, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err, "a rate limit pauses the run without failing it")
	assert.True(paused.Paused)
	require.NotNil(paused.RateLimit)
	assert.Equal(1, paused.Added, "the first page is archived before the limit")
	assert.Equal(1, paused.Remaining)
	assert.Equal("paused", runs(t, st, source.ID)[0].Status)
	state := resumeState(t, st, source.ID)
	require.Len(state.Queue, 1)
	assert.JSONEq(`1`, string(state.Queue[0].Continuation), "the place the fetch reached is kept")

	delete(f.failures, "i1")
	resumed, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.False(resumed.Paused)
	assert.Equal(1, resumed.Added)
	assert.Zero(resumed.Remaining)
	assert.Equal([]string{"i1@", "i1@1"}, f.fetches, "the second run continues from the second page")
	assert.Equal([]string{"", "1"}, f.cursors, "discovery continues after the first run's last page")
}

func TestSyncPausesDiscoveryWhenTheSchedulerAsks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{
		{ref("i1", "acme", "2026-11-07T10:00:00Z")},
		{ref("i2", "acme", "2026-11-06T10:00:00Z")},
	}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.threads["i2"] = [][]Post{{post("p2", "issue", ann, "Two", "Body.", "2026-11-01T10:00:00Z")}}
	f.pages = append(f.pages, []ThreadRef{ref("i3", "acme", "2026-11-05T10:00:00Z")})
	f.threads["i3"] = [][]Post{{post("p3", "issue", ann, "Three", "Body.", "2026-11-01T10:00:00Z")}}
	ctx, preempt := jobctx.WithPreemption(t.Context())
	// The scheduler asks while the second page is being read: that page is
	// queued and saved, then the run yields.
	f.afterPage = func(i int) {
		if i == 0 {
			preempt()
		}
	}

	paused, err := NewSyncer(st, f, nil).Sync(ctx, source.ID, Options{})
	require.NoError(err)
	assert.True(paused.Paused)
	assert.Zero(paused.Archived, "a yielding run archives nothing more")
	assert.Equal("paused", runs(t, st, source.ID)[0].Status)
	state := resumeState(t, st, source.ID)
	assert.JSONEq(`2`, string(state.Cursor))
	require.Len(state.Queue, 2)

	f.afterPage = nil
	resumed, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Equal(3, resumed.Archived, "the two queued threads and the third page's")
	assert.Equal([]string{"", "2"}, f.cursors)
}

func TestSyncDropsAThreadWhoseOwnerIsNowExcluded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "Moved", "Body.", "2026-11-01T10:00:00Z")}}
	f.owners["i1"] = "secret"

	summary, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{Owners: NewOwnerFilter(nil, []string{"secret"})})
	require.NoError(err)
	assert.Equal(1, summary.Excluded)
	assert.Zero(summary.Remaining)
	assert.Empty(f.fetches, "the moved repository's thread is never fetched")
}

func TestSyncDropsGoneThreadsAndRetriesFailedOnes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{
		ref("gone", "acme", "2026-11-07T12:00:00Z"),
		ref("flaky", "acme", "2026-11-07T11:00:00Z"),
		ref("fine", "acme", "2026-11-07T10:00:00Z"),
	}}
	f.threads["flaky"] = [][]Post{{post("p1", "issue", ann, "Flaky", "Body.", "2026-11-01T10:00:00Z")}}
	f.threads["fine"] = [][]Post{{post("p2", "issue", ann, "Fine", "Body.", "2026-11-01T10:00:00Z")}}
	f.failures["flaky"] = fetchFailure{page: 0, err: errors.New("server error")}

	summary, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Equal(1, summary.Unavailable)
	assert.Equal(1, summary.Failed)
	assert.Equal(1, summary.Archived)
	assert.Equal(1, summary.Remaining, "the failed thread waits for the next sync")
	assert.Equal("completed", runs(t, st, source.ID)[0].Status)

	f.failures["fine"] = fetchFailure{page: 0, err: errors.New("server error")}
	f.pages = nil
	_, err = NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.ErrorIs(err, ErrEveryThreadFailed)
}

func TestSyncRestartsDiscoveryWhenTheOwnerFilterChanges(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{}, {}}
	syncer := NewSyncer(st, f, nil)

	_, err := syncer.Sync(t.Context(), source.ID, Options{Owners: NewOwnerFilter(nil, []string{"secret"})})
	require.NoError(err)
	_, err = syncer.Sync(t.Context(), source.ID, Options{Owners: NewOwnerFilter(nil, []string{"secret"})})
	require.NoError(err)
	_, err = syncer.Sync(t.Context(), source.ID, Options{Owners: NewOwnerFilter(nil, nil)})
	require.NoError(err)
	_, err = syncer.Sync(t.Context(), source.ID, Options{Full: true})
	require.NoError(err)
	assert.Equal(t, []string{"", "2", "", ""}, f.cursors)
}

func TestSyncGivesDiscoveryTheArchivedThreads(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z"), ref("i2", "acme", "2026-11-06T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.threads["i2"] = [][]Post{{post("p2", "issue", ann, "Two", "Body.", "2026-11-01T10:00:00Z")}}
	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	var oldest []KnownThread
	f.discover = func(ctx context.Context, known KnownThreads, _ func([]ThreadRef, []byte) error) error {
		var err error
		oldest, err = known.Oldest(ctx, 1)
		return err
	}
	_, err = NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	require.Len(oldest, 1)
	assert.Contains(t, []string{"i1", "i2"}, oldest[0].Ref.Key)
}

func TestSyncRejectsATokenOfAnotherAccount(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.account = Account{Kind: AuthorAccount, ID: "4004", Login: "someone-else"}

	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.ErrorContains(t, err, "someone-else")
}

func TestSyncKeepsTheQueueWhenInterrupted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z"), ref("i2", "acme", "2026-11-06T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.threads["i2"] = [][]Post{{post("p2", "issue", ann, "Two", "Body.", "2026-11-01T10:00:00Z")}}
	ctx, cancel := context.WithCancel(t.Context())
	f.afterPage = func(int) { cancel() }

	_, err := NewSyncer(st, f, nil).Sync(ctx, source.ID, Options{})
	require.ErrorIs(err, context.Canceled)
	assert.Len(resumeState(t, st, source.ID).Queue, 2, "the queue survives the interruption")

	f.afterPage = nil
	resumed, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Equal(2, resumed.Archived)
}

func TestQueueKeepsOneEntryPerThreadNewestFirst(t *testing.T) {
	assert := assert.New(t)
	state := syncState{Version: stateVersion}
	older := ref("a", "acme", "2026-11-01T00:00:00Z")
	older.Reason = "mention"
	assert.Equal(1, state.queue([]ThreadRef{older}))
	state.Queue[0].Continuation = []byte(`3`)
	state.Queue[0].ReadAt = older.UpdatedAt.Add(time.Hour)

	same := ref("a", "acme", "2026-11-01T00:00:00Z")
	other := ref("b", "acme", "2026-11-02T00:00:00Z")
	assert.Equal(1, state.queue([]ThreadRef{same, other}))
	assert.Equal("b", state.Queue[0].Ref.Key, "newest first")
	assert.JSONEq(`3`, string(state.Queue[1].Continuation), "an unchanged thread keeps its place")
	assert.Equal("mention", state.Queue[1].Ref.Reason, "a search hit keeps the notification's reason")

	newer := ref("a", "acme", "2026-11-03T00:00:00Z")
	assert.Zero(state.queue([]ThreadRef{newer}))
	assert.Equal("a", state.Queue[0].Ref.Key)
	assert.Nil(state.Queue[0].Continuation, "a changed thread is fetched from the start")
	assert.Equal("mention", state.Queue[0].Ref.Reason)

	state.Queue[0].Continuation = []byte(`2`)
	state.Queue[0].ReadAt = newer.UpdatedAt.Add(500 * time.Millisecond)
	assert.Zero(state.queue([]ThreadRef{newer}))
	assert.Nil(state.Queue[0].Continuation,
		"a thread whose first page was read within a second of its update may have changed since, so it starts over")
}

func TestParseInstance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, tc := range []struct {
		raw, instance, base string
		bad                 bool
	}{
		{raw: "https://GitHub.com/", instance: "github.com", base: "https://github.com"},
		{raw: "https://git.example.com:443/gitea/", instance: "git.example.com/gitea", base: "https://git.example.com/gitea"},
		{raw: "http://nas.local:3000", instance: "nas.local:3000", base: "http://nas.local:3000"},
		{raw: "http://[::1]:3000/", instance: "[::1]:3000", base: "http://[::1]:3000"},
		{raw: "https://[FD00::1]", instance: "[fd00::1]", base: "https://[fd00::1]"},
		{raw: "https://git.example.com/a/../gitlab/./", instance: "git.example.com/gitlab", base: "https://git.example.com/gitlab"},
		{raw: "https://git.example.com//forge//gitea", instance: "git.example.com/forge/gitea", base: "https://git.example.com/forge/gitea"},
		{raw: "https://git.example.com/a/..", instance: "git.example.com", base: "https://git.example.com"},
		{raw: "https://git.example.com/my%20forge/", instance: "git.example.com/my%20forge", base: "https://git.example.com/my%20forge"},
		{raw: "ftp://example.com", bad: true},
		{raw: "https://user:pass@example.com", bad: true},
		{raw: "https://example.com/?x=1", bad: true},
	} {
		instance, base, err := ParseInstance(tc.raw)
		if tc.bad {
			require.Error(err, tc.raw)
			continue
		}
		require.NoError(err, tc.raw)
		assert.Equal(Instance(tc.instance), instance, tc.raw)
		assert.Equal(tc.base, base.String(), tc.raw)
		again, _, err := ParseInstance(base.String())
		require.NoError(err, tc.raw)
		assert.Equal(instance, again, "the base URL names the same instance: %s", tc.raw)
	}
}

func TestDecodeStateRejectsAnotherVersion(t *testing.T) {
	data, err := json.Marshal(syncState{Version: stateVersion + 1})
	require.NoError(t, err)
	_, err = decodeState(string(data))
	assert.Error(t, err)
}

func TestSyncKeepsEvidenceThatDiffersOnlyInFieldOrder(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	first := post("p1", "issue", ann, "Title", "Body.", "2026-11-01T10:00:00Z")
	first.Raw = []byte(`{"id":"p1","author":{"login":"ann","id":2002}}`)
	f.threads["i1"] = [][]Post{{first}}
	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	reordered := first
	reordered.Raw = []byte(`{"author":{"id":2002,"login":"ann"},"id":"p1"}`)
	f.threads["i1"] = [][]Post{{reordered}}
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-08T10:00:00Z")}}
	again, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{Full: true})
	require.NoError(err)
	assert.Zero(t, again.Updated, "the same JSON in another order is the same evidence")
}

func TestSyncKeepsTheAccountsPostsItsOwnWhenItsEmailJoinsAPerson(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	withEmail := octo
	withEmail.Email = "octo@example.com"
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{
		post("p1", "issue", ann, "Title", "Body.", "2026-11-01T10:00:00Z"),
		post("c1", "issue_comment", withEmail, "", "Mine.", "2026-11-02T10:00:00Z"),
	}}

	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	fromMe := map[string]bool{}
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(
		`SELECT source_message_id, is_from_me FROM messages WHERE source_id = ?`), source.ID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var mine bool
		require.NoError(rows.Scan(&id, &mine))
		fromMe[id] = mine
	}
	require.NoError(rows.Err())
	assert.Equal(t, map[string]bool{"p1": false, "c1": true}, fromMe,
		"the account's email joins it to a person; its posts stay the account's own")
}

func TestAPIWaitsUntilAShortRateLimitResets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls = append(calls, time.Now())
		if len(calls) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	require.NoError(err)
	api := &API{Base: base, Client: server.Client(), Limited: func(resp *http.Response) (time.Time, bool) {
		// The forge names only when its limit resets: two seconds from now.
		return time.Now().Add(2 * time.Second), resp.StatusCode == http.StatusTooManyRequests
	}}

	_, err = api.GetJSON(t.Context(), "/thing", nil, nil, &struct{}{})
	require.NoError(err)
	require.Len(calls, 2)
	assert.GreaterOrEqual(calls[1].Sub(calls[0]), 2*time.Second, "the retry waits for the reset, not a short backoff")
}

func TestAnEarlyFailureKeepsTheResumeStateOfTheRunBefore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}, {post("c1", "issue_comment", ann, "", "Two.", "2026-11-02T10:00:00Z")}}
	f.failures["i1"] = fetchFailure{page: 1, err: &RateLimitError{Resource: "api", Reset: time.Now().Add(time.Hour)}}
	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	broken := &failingAccountForge{fakeForge: f}
	_, err = NewSyncer(st, broken, nil).Sync(t.Context(), source.ID, Options{})
	require.Error(err)

	delete(f.failures, "i1")
	resumed, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Equal(1, resumed.Added, "the run after the failure resumes the thread at its second page")
	assert.Contains(f.fetches, "i1@1")
}

// failingAccountForge is a forge whose account check fails.
type failingAccountForge struct{ *fakeForge }

func (f *failingAccountForge) Account(context.Context) (Account, error) {
	return Account{}, errors.New("GitHub is down")
}

func TestACheckpointInTheMiddleOfAThreadKeepsItsPlace(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{
		{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")},
		{post("c1", "issue_comment", ann, "", "Two.", "2026-11-02T10:00:00Z")},
		{post("c2", "issue_comment", ann, "", "Three.", "2026-11-03T10:00:00Z")},
	}
	syncer := NewSyncer(st, f, nil)
	var midThread syncState
	f.afterPost = func(_ string, index int) {
		if index == 1 {
			// What a sync that died here would resume from.
			midThread = resumeState(t, st, source.ID)
		}
	}
	_, err := syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	require.Len(midThread.Queue, 1)
	assert.JSONEq(t, `2`, string(midThread.Queue[0].Continuation), "the checkpoint holds the page the fetch reached")
}

func TestAFullSyncFetchesQueuedThreadsFromTheirStart(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}, {post("c1", "issue_comment", ann, "", "Two.", "2026-11-02T10:00:00Z")}}
	f.failures["i1"] = fetchFailure{page: 1, err: &RateLimitError{Resource: "api", Reset: time.Now().Add(time.Hour)}}
	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	delete(f.failures, "i1")
	_, err = NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{Full: true})
	require.NoError(err)
	assert.Equal(t, []string{"i1@", "i1@"}, f.fetches, "the full sync starts the thread over")
}

func TestASchedulerStopKeepsAnIndependentFailure(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z"), ref("i2", "acme", "2026-11-06T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.threads["i2"] = [][]Post{{post("p2", "issue", ann, "Two", "Body.", "2026-11-01T10:00:00Z")}}
	f.failures["i1"] = fetchFailure{page: 0, err: errors.New("server error")}
	ctx, cancel := context.WithCancelCause(t.Context())
	// The run's budget runs out as it reaches the second thread.
	budget := &cancelAtOwnerForge{fakeForge: f, key: "i2", cancel: func() { cancel(jobctx.ErrRunBudgetExceeded) }}

	summary, err := NewSyncer(st, budget, nil).Sync(ctx, source.ID, Options{})
	require.ErrorIs(err, ErrEveryThreadFailed, "the budget ran out, but every thread the run tried had failed")
	assert.False(t, summary.Paused)
	assert.Equal(t, "failed", runs(t, st, source.ID)[0].Status)
}

// cancelAtOwnerForge stops the run's context when the core asks for one
// thread's owner, as the scheduler does when a run's budget runs out.
type cancelAtOwnerForge struct {
	*fakeForge

	key    string
	cancel func()
}

func (f *cancelAtOwnerForge) Owner(ctx context.Context, ref ThreadRef) (string, error) {
	if ref.Key == f.key {
		f.cancel()
		return "", ctx.Err()
	}
	return f.fakeForge.Owner(ctx, ref)
}

func TestAFailureAsTheBudgetRunsOutStillCounts(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.failures["i1"] = fetchFailure{page: 0, err: errors.New("server error")}
	ctx, cancel := context.WithCancelCause(t.Context())
	// The budget runs out just as the only thread's fetch fails on its own.
	budget := &cancelAfterFetchForge{fakeForge: f, cancel: func() { cancel(jobctx.ErrRunBudgetExceeded) }}

	_, err := NewSyncer(st, budget, nil).Sync(ctx, source.ID, Options{})
	require.ErrorIs(err, ErrEveryThreadFailed)
	assert.Len(t, resumeState(t, st, source.ID).Queue, 1, "the failed thread stays queued")
}

// cancelAfterFetchForge stops the run's context as a fetch returns.
type cancelAfterFetchForge struct {
	*fakeForge

	cancel func()
}

func (f *cancelAfterFetchForge) Fetch(ctx context.Context, ref ThreadRef, continuation []byte,
	page func(ThreadHead, []Post, []byte) error,
) error {
	defer f.cancel()
	return f.fakeForge.Fetch(ctx, ref, continuation, page)
}

func TestSyncFetchesNoThreadArchivedAtTheUpdateFound(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}, {ref("i2", "acme", "2026-11-06T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.threads["i2"] = [][]Post{{post("p2", "issue", ann, "Two", "Body.", "2026-11-01T10:00:00Z")}}
	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	// The forge's lists show both threads again, one changed since.
	f.pages = append(f.pages, []ThreadRef{ref("i1", "acme", "2026-11-07T10:00:00Z"), ref("i2", "acme", "2026-11-08T10:00:00Z")})
	again, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Equal(1, again.Found, "the unchanged thread is not queued")
	assert.Equal(1, again.Archived)

	full, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{Full: true})
	require.NoError(err)
	assert.Equal(2, full.Archived, "a full sync fetches every thread it finds")
}

func TestAFullSyncThatPausesResumesItsDiscoveryAsAFullOne(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	unchanged := ref("i2", "acme", "2026-11-06T10:00:00Z")
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}, {unchanged}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.threads["i2"] = [][]Post{{post("p2", "issue", ann, "Two", "Body.", "2026-11-01T10:00:00Z")}}
	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	// A full sync pauses after discovery's first page. Its second page shows
	// i2 at the update it is archived at, with a reason it now has.
	mentioned := unchanged
	mentioned.Reason = "mention"
	f.pages[1] = []ThreadRef{mentioned}
	ctx, preempt := jobctx.WithPreemption(t.Context())
	f.afterPage = func(i int) {
		if i == 0 {
			preempt()
		}
	}
	paused, err := NewSyncer(st, f, nil).Sync(ctx, source.ID, Options{Full: true})
	require.NoError(err)
	require.True(paused.Paused)

	f.afterPage = nil
	_, err = NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	archived, err := (&knownThreads{store: st, sourceID: source.ID}).all(t.Context())
	require.NoError(err)
	assert.Equal("mention", archived["i2"].Ref.Reason,
		"the resumed discovery is a full one, so it queues what it finds even at the archived update")
	assert.False(resumeState(t, st, source.ID).Full)
}

func TestAPIFollowsRedirectsOnlyOnItsInstance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// A service on this machine, which no forge may send a sync to.
	var local []string
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local = append(local, r.URL.Path)
		_, _ = w.Write([]byte(`{"secret":true}`))
	}))
	t.Cleanup(service.Close)
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/moved":
			http.Redirect(w, r, "/thing", http.StatusMovedPermanently)
		case "/away":
			http.Redirect(w, r, service.URL+"/admin", http.StatusFound)
		case "/numeric":
			// 2130706433 is 127.0.0.1 written as one number.
			http.Redirect(w, r, strings.Replace(service.URL, "127.0.0.1", "2130706433", 1)+"/admin", http.StatusFound)
		default:
			_, _ = w.Write([]byte(`{"name":"thing"}`))
		}
	}))
	t.Cleanup(instance.Close)
	base, err := url.Parse(instance.URL)
	require.NoError(err)
	api := &API{Base: base, Client: NewHTTPClient(time.Minute)}

	var thing struct {
		Name string `json:"name"`
	}
	_, err = api.GetJSON(t.Context(), "/moved", nil, nil, &thing)
	require.NoError(err, "a move on the instance is followed")
	assert.Equal("thing", thing.Name)

	for _, path := range []string{"/away", "/numeric"} {
		_, err = api.GetJSON(t.Context(), path, nil, nil, &struct{}{})
		require.ErrorContains(err, "redirected outside its instance", path)
	}
	assert.Empty(local, "nothing reaches the other host")
}

func TestAPIAcceptsTheLinksOfItsInstanceOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, tc := range []struct {
		base, link string
		accepted   bool
	}{
		{"https://forge.example.test/api/v4", "https://forge.example.test/api/v4/issues?page=2", true},
		{"https://forge.example.test/api/v4", "https://FORGE.example.test/api/v4/issues?page=2", true},
		// An http instance that redirects to https returns https links.
		{"http://forge.example.test/api/v4", "https://forge.example.test/api/v4/issues?page=2", true},
		{"https://forge.example.test/api/v4", "http://forge.example.test/api/v4/issues?page=2", false},
		{"https://forge.example.test/api/v4", "https://other.example.test/api/v4/issues?page=2", false},
		{"http://forge.example.test/api/v4", "https://forge.example.test:8443/api/v4/issues?page=2", false},
	} {
		base, err := url.Parse(tc.base)
		require.NoError(err)
		api := &API{Base: base}
		got, err := api.URL(tc.link, nil)
		if tc.accepted {
			assert.NoError(err, tc.base+" "+tc.link)
			assert.Equal(tc.link, got)
		} else {
			assert.ErrorContains(err, "outside its API", tc.base+" "+tc.link)
		}
	}
}

func TestAThreadFetchedAcrossSyncsIsKnownFromWhenItsFirstPageWasRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{
		{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")},
		{post("c1", "issue_comment", ann, "", "Two.", "2026-11-02T10:00:00Z")},
	}
	read := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clock := read
	syncer := NewSyncer(st, f, nil)
	syncer.now = func() time.Time { return clock }

	// The first sync reads the opening post, then reaches a rate limit.
	f.failures["i1"] = fetchFailure{page: 1, err: &RateLimitError{Resource: "core", Reset: read.Add(time.Hour)}}
	paused, err := syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	require.True(paused.Paused)

	// An hour later the next sync finishes the thread.
	delete(f.failures, "i1")
	clock = read.Add(time.Hour)
	_, err = syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	var known []KnownThread
	f.discover = func(ctx context.Context, threads KnownThreads, _ func([]ThreadRef, []byte) error) error {
		return threads.Each(ctx, func(k KnownThread) error {
			known = append(known, k)
			return nil
		})
	}
	_, err = syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	require.Len(known, 1)
	assert.True(read.Equal(known[0].ReadAt), "an edit to the opening post during the pause is after ReadAt")
	assert.True(read.Add(time.Hour).Equal(known[0].CheckedAt))
}

func TestAThreadTitleHoldingNULIsArchived(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	f.titles = map[string]string{"i1": "[acme/widgets] Broken\x00 title (#1)"}

	summary, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	require.Equal(1, summary.Archived, "the thread is archived, not left failing in the queue")
	require.Zero(summary.Failed)
}

func TestAFullSyncFetchesAnArchivedThreadNoListShowsAnyMore(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	f.pages = [][]ThreadRef{{ref("i1", "acme", "2026-11-07T10:00:00Z")}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	_, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	// The forge lists the thread nowhere now, as a release whose
	// notification is gone.
	f.discover = func(context.Context, KnownThreads, func([]ThreadRef, []byte) error) error { return nil }
	full, err := NewSyncer(st, f, nil).Sync(t.Context(), source.ID, Options{Full: true})
	require.NoError(err)
	require.Equal(1, full.Archived, "a full sync fetches every archived thread")
}

func TestAThreadChangedInTheSecondItWasFetchedIsFetchedAgain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	update := "2026-11-07T10:00:00Z"
	f.pages = [][]ThreadRef{{ref("i1", "acme", update)}}
	f.threads["i1"] = [][]Post{{post("p1", "issue", ann, "One", "Body.", "2026-11-01T10:00:00Z")}}
	at, err := time.Parse(time.RFC3339, update)
	require.NoError(err)
	clock := at.Add(500 * time.Millisecond)
	syncer := NewSyncer(st, f, nil)
	syncer.now = func() time.Time { return clock }
	_, err = syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)

	// Later in the same second the thread changed again; the forge reports
	// the same whole-second update.
	f.pages = append(f.pages, []ThreadRef{ref("i1", "acme", update)})
	clock = at.Add(time.Hour)
	again, err := syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Equal(1, again.Archived, "a copy read within a second of the update may miss part of it")

	f.pages = append(f.pages, []ThreadRef{ref("i1", "acme", update)})
	settled, err := syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Zero(settled.Found, "a copy read well after the update holds it")
}

func TestAPausedThreadEditedInTheSecondItWasReadStartsOver(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newForgeSource(t, st)
	f := newFakeForge()
	update := "2026-11-07T10:00:00Z"
	f.pages = [][]ThreadRef{{ref("i1", "acme", update)}}
	f.threads["i1"] = [][]Post{
		{post("p1", "issue", ann, "Title", "Body.", "2026-11-01T10:00:00Z")},
		{post("c1", "issue_comment", octo, "", "Reply.", "2026-11-02T10:00:00Z")},
	}
	at, err := time.Parse(time.RFC3339, update)
	require.NoError(err)
	clock := at.Add(500 * time.Millisecond)
	syncer := NewSyncer(st, f, nil)
	syncer.now = func() time.Time { return clock }
	f.failures["i1"] = fetchFailure{page: 1, err: &RateLimitError{Resource: "graphql", Reset: clock.Add(time.Hour)}}
	paused, err := syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	require.True(paused.Paused)

	// Later in the same second the opening post was edited, after the
	// paused fetch read it. Discovery reports the same update once more,
	// then moves past it.
	f.threads["i1"][0][0] = post("p1", "issue", ann, "Title", "Edited.", "2026-11-01T10:00:00Z")
	delete(f.failures, "i1")
	f.pages = append(f.pages, []ThreadRef{ref("i1", "acme", update), ref("i2", "acme", "2026-11-07T11:00:00Z")})
	f.threads["i2"] = [][]Post{{post("p2", "issue", ann, "Other", "Body.", "2026-11-07T11:00:00Z")}}
	clock = at.Add(time.Hour)
	resumed, err := syncer.Sync(t.Context(), source.ID, Options{})
	require.NoError(err)
	assert.Equal([]string{"i1@", "i2@", "i1@"}, f.fetches, "the paused thread starts over instead of continuing")
	assert.Equal(1, resumed.Updated, "the edited opening post")
	assert.Equal(2, resumed.Added, "the reply, and the other thread's post")
}
