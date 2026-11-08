package forge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
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
