package kataissues_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kata "go.kenn.io/kata"
	katagen "go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type people map[int64][]string

func (p people) ListPersonUIDsContext(_ context.Context, id int64) ([]string, error) {
	return p[id], nil
}

// kataServer fronts a real Kata service. While dropCreates is set it lets a
// create reach Kata but loses the response; creates counts every create sent.
type kataServer struct {
	*httptest.Server

	service     *kata.Service
	projectID   int64
	dropCreates atomic.Bool
	// failMetadataWrite fails the metadata write it counts down to, without
	// forwarding it: 1 fails the next one, 2 the one after.
	failMetadataWrite atomic.Int32
	// refuseCreates answers every create with a 409 unrelated to idempotency.
	refuseCreates atomic.Bool
	// refuseMetadata answers every metadata write with a 409 unrelated to its guard.
	refuseMetadata atomic.Bool
	// failComment fails the next comment post without forwarding it.
	failComment atomic.Bool
	// mismatchComment answers the next comment post as Kata answers a key
	// reused under another actor, without forwarding it.
	mismatchComment atomic.Bool
	// afterMetadata runs once, after the next metadata write lands and before
	// its caller hears back.
	afterMetadata atomic.Pointer[func()]
	// editFirst lands an unrelated metadata edit before every metadata write.
	editFirst atomic.Bool
	creates   atomic.Int32
	mu        sync.Mutex
	lost      []byte
}

func newKataServer(t *testing.T) *kataServer {
	t.Helper()
	native := katatest.New(t)
	service := native.Service
	server := &kataServer{service: service, projectID: native.ProjectID}
	server.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
			server.creates.Add(1)
			if server.refuseCreates.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"status":409,"error":{"code":"federated_read_only","message":"project is read-only"}}`))
				return
			}
			if server.dropCreates.Load() {
				recorder := httptest.NewRecorder()
				r.Header.Del("Accept-Encoding")
				service.Handler().ServeHTTP(recorder, r)
				server.mu.Lock()
				server.lost = recorder.Body.Bytes()
				server.mu.Unlock()
				http.Error(w, "connection lost", http.StatusBadGateway)
				return
			}
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/metadata") && server.refuseMetadata.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"status":409,"error":{"code":"federated_read_only","message":"project is read-only"}}`))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/metadata") && server.editFirst.Load() {
			edit := httptest.NewRequestWithContext(r.Context(), http.MethodPost, r.URL.Path, strings.NewReader(`{"actor":"someone-else","patch":{"other.note":"edited"}}`))
			edit.Header.Set("Authorization", "Bearer "+katatest.Token)
			edit.Header.Set("Content-Type", "application/json")
			service.Handler().ServeHTTP(httptest.NewRecorder(), edit)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") && server.mismatchComment.CompareAndSwap(true, false) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"status":409,"error":{"code":"idempotency_mismatch","message":"key reused"}}`))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") && server.failComment.CompareAndSwap(true, false) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/metadata") && server.failMetadataWrite.Load() > 0 && server.failMetadataWrite.Add(-1) == 0 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/metadata") {
			if hook := server.afterMetadata.Swap(nil); hook != nil {
				recorder := httptest.NewRecorder()
				service.Handler().ServeHTTP(recorder, r)
				(*hook)()
				maps.Copy(w.Header(), recorder.Header())
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
		}
		service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *kataServer) endpoint() katatest.Endpoint {
	return katatest.Endpoint{URL: s.URL, Client: s.Client(), ProjectID: s.projectID}
}

type fixture struct {
	store   *storetest.Fixture
	kata    *kataServer
	service *kataissues.Service
	message int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: storetest.New(t), kata: newKataServer(t)}
	f.message = f.store.CreateMessage("evidence-message")
	_, err := f.store.Store.DB().Exec(f.store.Store.Rebind("INSERT INTO message_bodies (message_id,body_text) VALUES (?,?)"), f.message,
		strings.Repeat("Earlier context. ", 150)+"Please send the revised budget by Friday.")
	require.NoError(t, err)
	f.service = f.connect(t)
	return f
}

func (f *fixture) connect(t *testing.T) *kataissues.Service {
	t.Helper()
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: f.kata.URL, APIKey: katatest.Token, HTTPClient: f.kata.Client(), DefaultProject: "example"})
	require.NoError(t, err)
	return &kataissues.Service{Archive: f.store.Store, Kata: client, Evidence: kataevidence.New(f.store.Store), People: people{7: {"person-uid-7"}}, Project: "example"}
}

func (f *fixture) evidence(t *testing.T, start int) kataevidence.Evidence {
	t.Helper()
	prepared, err := kataevidence.New(f.store.Store).Prepare(t.Context(), []kataevidence.Selector{{Kind: "message", MessageID: f.message, StartRune: &start, MaxChars: new(kataevidence.MaxChars)}})
	require.NoError(t, err)
	return prepared[0]
}

func TestCreateSavesPreparedQuotation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	prepared := f.evidence(t, 2000)

	created, err := f.service.Create(t.Context(), "key-a", kataissues.CreateInput{Title: "Send the revised budget", Brief: "Follow up with finance.", PersonID: new(int64(7)), Evidence: []kataevidence.Reference{prepared.Reference}})
	require.NoError(err)
	assert.False(created.Replayed)
	assert.Contains(created.Issue.Body, "> "+prepared.Excerpt)
	assert.Equal("person-uid-7", created.Issue.Metadata[personagenda.PersonMetadataKey])
	assert.Equal("agenda", created.Issue.Metadata[personagenda.ListMetadataKey])
	entries := envelopeOf(t, created.Issue).Entries
	require.Len(entries, 1)
	assert.Equal(prepared.ID, entries[0].ID)

	// Requests the service refuses before anything reaches Kata.
	sent := f.kata.creates.Load()
	input := kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{prepared.Reference}}
	_, err = f.service.Create(t.Context(), "key-list", kataissues.CreateInput{Title: input.Title, List: "follow up", Evidence: input.Evidence})
	require.ErrorIs(err, kataissues.ErrInvalidRequest, "a list needs a person")
	for state, want := range map[kataevidence.State]error{kataevidence.Unprocessed: kataevidence.ErrUnprocessed, kataevidence.Unsupported: kataevidence.ErrUnsupported} {
		service := f.connect(t)
		service.Evidence = resolvedAs(state)
		_, err = service.Create(t.Context(), "key-state", input)
		require.ErrorIs(err, want)
	}
	assert.Equal(sent, f.kata.creates.Load())
}

// resolvedAs reports every citation in one unavailable state.
type resolvedAs kataevidence.State

func (state resolvedAs) Resolve(context.Context, kataevidence.Reference) (kataevidence.Resolution, error) {
	return kataevidence.Resolution{State: kataevidence.State(state)}, nil
}

// A retry under the same key answers with the issue the key filed, however it
// changed since, and names it when the request itself changed.
func TestCreateRetryUnderTheSameKey(t *testing.T) {
	f := newFixture(t)
	t.Run("same request replays, a changed one conflicts", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		input := kataissues.CreateInput{Title: "Send the revised budget", Brief: "Follow up with finance.", PersonID: new(int64(7)), Evidence: []kataevidence.Reference{f.evidence(t, 2000).Reference}}
		created, err := f.service.Create(t.Context(), "key-a", input)
		require.NoError(err)
		sent := f.kata.creates.Load()
		input.Title = " Send the revised budget "
		again, err := f.service.Create(t.Context(), "key-a", input)
		require.NoError(err)
		assert.True(again.Replayed)
		assert.Equal(created.Issue.UID, again.Issue.UID)
		_, err = f.service.Create(t.Context(), "key-a", kataissues.CreateInput{Title: "A different title", Evidence: input.Evidence})
		require.ErrorIs(err, kataissues.ErrIdempotencyConflict)
		assert.Equal(sent, f.kata.creates.Load())
	})
	t.Run("lost response, then the issue closed", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		input := kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{f.evidence(t, 0).Reference}}
		f.kata.dropCreates.Store(true)
		_, err := f.service.Create(t.Context(), "key-lost", input)
		require.Error(err)
		f.kata.dropCreates.Store(false)
		var lost struct {
			Issue struct {
				UID string `json:"uid"`
			} `json:"issue"`
		}
		require.NoError(json.Unmarshal(f.kata.lost, &lost))
		require.NotEmpty(lost.Issue.UID)
		f.kata.endpoint().CloseIssue(t, lost.Issue.UID)

		// Kata's own replay window may have passed by now; the marker lookup
		// must answer the retry without another create reaching Kata.
		sent := f.kata.creates.Load()
		retried, err := f.service.Create(t.Context(), "key-lost", input)
		require.NoError(err)
		assert.True(retried.Replayed)
		assert.Equal(lost.Issue.UID, retried.Issue.UID)
		assert.Equal("closed", retried.Issue.Status)
		assert.Equal(sent, f.kata.creates.Load())
	})
	t.Run("issue moved to another project", func(t *testing.T) {
		require := require.New(t)
		_, err := f.kata.service.EnsureProject(t.Context(), kata.ProjectSpec{UID: "01HZNQ7VFPK1XGD8R5MABCD4EY", Name: "other"})
		require.NoError(err)
		input := kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{f.evidence(t, 0).Reference}}
		created, err := f.service.Create(t.Context(), "key-moved", input)
		require.NoError(err)
		require.NotContains(created.Issue.Metadata, personagenda.ListMetadataKey, "an issue for no one belongs on no agenda")
		f.kata.endpoint().MoveIssue(t, created.Issue.UID, created.Issue.Revision, "01HZNQ7VFPK1XGD8R5MABCD4EY")

		again, err := f.service.Create(t.Context(), "key-moved", input)
		require.NoError(err)
		require.True(again.Replayed)
		require.Equal(created.Issue.UID, again.Issue.UID)
		require.Equal("other", again.Issue.Project)

		// The replayed ref names the issue's new project, and Link follows it there.
		linked, err := f.service.Link(t.Context(), again.Issue.QualifiedRef, []kataevidence.Reference{f.evidence(t, f.evidence(t, 0).NextRune).Reference})
		require.NoError(err)
		require.Equal(2, passagesOf(t, linked))
		_, err = f.service.Link(t.Context(), "missing#abcd", []kataevidence.Reference{f.evidence(t, 0).Reference})
		require.ErrorIs(err, taskclient.ErrNotFound)
	})
	t.Run("issue deleted in Kata", func(t *testing.T) {
		require := require.New(t)
		input := kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{f.evidence(t, 0).Reference}}
		created, err := f.service.Create(t.Context(), "key-deleted-issue", input)
		require.NoError(err)
		f.kata.endpoint().DeleteIssue(t, created.Issue.UID, created.Issue.QualifiedRef)
		_, err = f.service.Create(t.Context(), "key-deleted-issue", input)
		require.ErrorIs(err, kataissues.ErrIssueDeleted)
	})
	t.Run("Kata refuses for another reason", func(t *testing.T) {
		require := require.New(t)
		f.kata.refuseCreates.Store(true)
		defer f.kata.refuseCreates.Store(false)
		_, err := f.service.Create(t.Context(), "key-refused", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{f.evidence(t, 0).Reference}})
		require.ErrorIs(err, taskclient.ErrRequestRejected)
		require.NotErrorIs(err, kataissues.ErrIssueDeleted)
		require.ErrorContains(err, "federated_read_only")
	})
	t.Run("message body re-synced", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		before := f.evidence(t, 0)
		created, err := f.service.Create(t.Context(), "key-resync", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{before.Reference}})
		require.NoError(err)
		_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=body_text||? WHERE message_id=?"), " Signature added on re-sync.", f.message)
		require.NoError(err)
		after := f.evidence(t, 0)
		require.Equal(before.Excerpt, after.Excerpt)
		require.NotEqual(before.Reference.Message.BodySHA256, after.Reference.Message.BodySHA256)
		replayed, err := f.service.Create(t.Context(), "key-resync", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{after.Reference}})
		require.NoError(err)
		assert.True(replayed.Replayed)
		assert.Equal(created.Issue.UID, replayed.Issue.UID)
	})
}

func TestCreateConcurrentRetriesMakeOneIssue(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	input := kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{f.evidence(t, 0).Reference}}
	var wg sync.WaitGroup
	results := make([]kataissues.Result, 4)
	for i := range results {
		wg.Go(func() {
			result, err := f.service.Create(t.Context(), "key-shared", input)
			assert.NoError(t, err)
			results[i] = result
		})
	}
	wg.Wait()
	created := 0
	for _, result := range results {
		require.Equal(results[0].Issue.UID, result.Issue.UID)
		if !result.Replayed {
			created++
		}
	}
	require.Equal(1, created, "only the call that created the issue reports replayed=false")
}

func TestLinkAddsEachPassageOnceAsAComment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	first := f.evidence(t, 0)
	created, err := f.service.Create(t.Context(), "key-link", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{first.Reference}})
	require.NoError(err)
	later := f.evidence(t, first.NextRune)

	// The record and the comment land, then clearing the pending mark fails;
	// the retry clears it without posting the comment again.
	f.kata.failMetadataWrite.Store(2)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{later.Reference, first.Reference})
	require.Error(err)
	require.Len(commentsOf(t, f.kata, created.Issue.UID), 1)
	for range 2 {
		linked, err := f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{later.Reference, first.Reference})
		require.NoError(err)
		assert.Equal(2, passagesOf(t, linked))
	}
	comments := commentsOf(t, f.kata, created.Issue.UID)
	require.Len(comments, 1)
	assert.Contains(comments[0], "> "+later.Excerpt)
	assert.NotContains(comments[0], first.Excerpt)

	// The same failed clear retried under another actor: Kata refuses the
	// reused comment key, which means the comment landed.
	reused := f.evidence(t, 250)
	f.kata.failMetadataWrite.Store(2)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{reused.Reference})
	require.Error(err)
	f.kata.mismatchComment.Store(true)
	retried, err := f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{reused.Reference})
	require.NoError(err)
	assert.Zero(pendingOf(t, retried))
	require.False(f.kata.mismatchComment.Load(), "the retry posted the comment again")
	require.Len(commentsOf(t, f.kata, created.Issue.UID), 2)

	// The record lands before its comment; a failed comment leaves the passage
	// pending with its quote and a label bounded like any stored text.
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE messages SET subject=? WHERE id=?"), strings.Repeat("S", 600), f.message)
	require.NoError(err)
	third := f.evidence(t, later.NextRune)
	f.kata.failComment.Store(true)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{third.Reference})
	require.Error(err)
	stored, err := f.service.Kata.GetTask(t.Context(), katatest.Project, created.Issue.UID)
	require.NoError(err)
	require.Equal(4, passagesOf(t, stored))
	require.Len(commentsOf(t, f.kata, created.Issue.UID), 2)

	// Recorded sources are never read again, even rewritten or deleted.
	linked, err := f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{later.Reference})
	require.NoError(err)
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=? WHERE message_id=?"), "Rewritten body", f.message)
	require.NoError(err)
	again, err := f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{later.Reference})
	require.NoError(err)
	require.Equal(linked.Revision, again.Revision)
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?"), f.message)
	require.NoError(err)
	again, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{later.Reference, first.Reference})
	require.NoError(err)
	require.Equal(linked.Revision, again.Revision)

	// With the source gone, a retry posts the pending quote from the record and
	// clears the mark; repeating it then posts nothing.
	for range 2 {
		_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{third.Reference})
		require.NoError(err)
		comments = commentsOf(t, f.kata, created.Issue.UID)
		require.Len(comments, 3)
		assert.Contains(comments[2], "> "+strings.SplitN(third.Excerpt, "\n", 2)[0])
	}
	stored, err = f.service.Kata.GetTask(t.Context(), katatest.Project, created.Issue.UID)
	require.NoError(err)
	for _, entry := range envelopeOf(t, stored).Entries {
		assert.Nil(entry.Pending, "every comment landed, so nothing stays pending")
	}
}

func TestLinkSurvivesUnrelatedEditsBetweenReadAndWrite(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	first := f.evidence(t, 0)
	created, err := f.service.Create(t.Context(), "key-busy", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{first.Reference}})
	require.NoError(err)
	f.kata.editFirst.Store(true)
	linked, err := f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{f.evidence(t, first.NextRune).Reference})
	require.NoError(err)
	require.Equal(2, passagesOf(t, linked))
	require.Equal("edited", linked.Metadata["other.note"])
	f.kata.editFirst.Store(false)

	// Another caller's link lands between this link's record and its clear;
	// the clear keeps the other link's passage.
	mine, theirs := f.evidence(t, 1500), f.evidence(t, 2000)
	interleave := func() {
		_, err := f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{theirs.Reference})
		assert.NoError(t, err)
	}
	f.kata.afterMetadata.Store(&interleave)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{mine.Reference})
	require.NoError(err)
	final, err := f.service.Kata.GetTask(t.Context(), katatest.Project, created.Issue.UID)
	require.NoError(err)
	require.Equal(4, passagesOf(t, final))
	for _, entry := range envelopeOf(t, final).Entries {
		require.Nil(entry.Pending)
	}
	require.Len(commentsOf(t, f.kata, created.Issue.UID), 3)
}

func TestLinkCapsWhatAnIssueHolds(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	windows := func(from, to int) []kataevidence.Reference {
		out := make([]kataevidence.Reference, 0, to-from)
		for i := from; i < to; i++ {
			out = append(out, f.evidence(t, i).Reference)
		}
		return out
	}
	issue := func(key string, size int) kataissues.Result {
		created, err := f.service.Create(t.Context(), key, kataissues.CreateInput{Title: "Send the revised budget", Evidence: windows(0, kataevidence.MaxReferences)})
		require.NoError(err)
		_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, windows(kataevidence.MaxReferences, size))
		require.NoError(err)
		return created
	}
	full := issue("key-full", kataissues.MaxIssuePassages)
	almost := issue("key-almost", kataissues.MaxIssuePassages-1)

	// A passage whose comment has not landed still holds its slot, and a link
	// refused for the cap still posts its quote and clears it.
	pending := issue("key-pending", kataissues.MaxIssuePassages-1)
	f.kata.failComment.Store(true)
	_, err := f.service.Link(t.Context(), pending.Issue.QualifiedRef, windows(kataissues.MaxIssuePassages, kataissues.MaxIssuePassages+1))
	require.Error(err)
	orphaned := len(commentsOf(t, f.kata, pending.Issue.UID))
	_, err = f.service.Link(t.Context(), pending.Issue.QualifiedRef, windows(kataissues.MaxIssuePassages+1, kataissues.MaxIssuePassages+2))
	require.ErrorIs(err, kataissues.ErrIssueFull)
	require.Len(commentsOf(t, f.kata, pending.Issue.UID), orphaned+1)
	finished, err := f.service.Kata.GetTask(t.Context(), katatest.Project, pending.Issue.UID)
	require.NoError(err)
	require.Equal(kataissues.MaxIssuePassages, passagesOf(t, finished))
	require.Zero(pendingOf(t, finished))

	// A re-sync outside the cited ranges changes evidence IDs but not passages.
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=body_text||? WHERE message_id=?"), " Signature.", f.message)
	require.NoError(err)
	comments := len(commentsOf(t, f.kata, full.Issue.UID))
	linked, err := f.service.Link(t.Context(), full.Issue.QualifiedRef, windows(5, 6))
	require.NoError(err)
	require.Equal(kataissues.MaxIssuePassages, passagesOf(t, linked))
	require.Len(commentsOf(t, f.kata, full.Issue.UID), comments)

	// Two references to one passage fill the last slot once.
	service := f.connect(t)
	service.Evidence = fixedText("Please send the revised budget by Friday.")
	archive, err := f.store.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	comments = len(commentsOf(t, f.kata, almost.Issue.UID))
	linked, err = service.Link(t.Context(), almost.Issue.QualifiedRef, []kataevidence.Reference{chunkReference(archive, f.message, "extraction-old", "b"), chunkReference(archive, f.message, "extraction-new", "c")})
	require.NoError(err)
	require.Equal(kataissues.MaxIssuePassages, passagesOf(t, linked))
	require.Len(commentsOf(t, f.kata, almost.Issue.UID), comments+1)

	// Each re-extraction gives one passage another evidence ID; those count
	// toward a separate, larger cap.
	extraction := func(i int) kataevidence.Reference {
		return chunkReference(archive, f.message, fmt.Sprintf("extraction-%03d", i), "b")
	}
	aliased, err := service.Create(t.Context(), "key-aliases", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{extraction(0)}})
	require.NoError(err)
	for from := 1; from < kataissues.MaxIssueEntries; from += kataevidence.MaxReferences {
		var batch []kataevidence.Reference
		for i := from; i < min(from+kataevidence.MaxReferences, kataissues.MaxIssueEntries); i++ {
			batch = append(batch, extraction(i))
		}
		_, err = service.Link(t.Context(), aliased.Issue.QualifiedRef, batch)
		require.NoError(err)
	}
	filled, err := service.Kata.GetTask(t.Context(), katatest.Project, aliased.Issue.UID)
	require.NoError(err)
	require.Len(envelopeOf(t, filled).Entries, kataissues.MaxIssueEntries)
	_, err = service.Link(t.Context(), aliased.Issue.QualifiedRef, []kataevidence.Reference{extraction(kataissues.MaxIssueEntries)})
	require.ErrorIs(err, kataissues.ErrIssueFull)
	_, err = f.service.Link(t.Context(), aliased.Issue.QualifiedRef, []kataevidence.Reference{f.evidence(t, 0).Reference})
	require.ErrorIs(err, kataissues.ErrIssueFull)
	require.Empty(commentsOf(t, f.kata, aliased.Issue.UID))
}

// fixedText resolves every citation to the same words, standing in for a
// file whose re-extraction produced identical chunk text.
type fixedText string

func (text fixedText) Resolve(_ context.Context, ref kataevidence.Reference) (kataevidence.Resolution, error) {
	ref, err := kataevidence.Canonicalize(ref)
	return kataevidence.Resolution{State: kataevidence.Available, Evidence: kataevidence.Evidence{ID: kataevidence.ID(ref), Passage: kataevidence.PassageID(ref, string(text)), Reference: ref, Excerpt: string(text)}}, err
}

// chunkReference cites the same chunk range; extraction and checksum vary as
// they do when a file is processed again.
func chunkReference(archive string, message int64, extraction, checksum string) kataevidence.Reference {
	return kataevidence.Reference{Version: kataevidence.Version, Kind: "document_chunk", ArchiveUID: archive, MessageID: message, SourceType: "email", SourceIdentifier: "inbox@example.com",
		SourceMessageID: "m1", AttachmentID: 3, OccurrenceKey: "occurrence", DocumentChunk: &kataevidence.DocumentReference{CanonicalBlobHash: strings.Repeat("a", 64), ExtractionID: extraction,
			ManifestChecksum: strings.Repeat(checksum, 64), ChunkKey: "page:000000:000000-000041", ChunkChecksum: strings.Repeat(checksum, 64), StartRune: 0, EndRune: 41}}
}

func TestQuotesFollowTheCallersOrder(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	windows := []kataevidence.Evidence{f.evidence(t, 0), f.evidence(t, 1000), f.evidence(t, 2000), f.evidence(t, 1500)}
	created, err := f.service.Create(t.Context(), "key-order", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{windows[1].Reference, windows[0].Reference, windows[1].Reference}})
	require.NoError(err)
	second, first := strings.Index(created.Issue.Body, windows[1].Excerpt), strings.Index(created.Issue.Body, windows[0].Excerpt)
	require.True(second >= 0 && first > second, "the body quotes in the order given")

	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{windows[2].Reference, windows[3].Reference})
	require.NoError(err)
	comments := commentsOf(t, f.kata, created.Issue.UID)
	require.Len(comments, 2)
	require.Contains(comments[0], windows[2].Excerpt)
	require.Contains(comments[1], windows[3].Excerpt)
}

func TestAgendaListsMaximumSizeIssues(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	// Control characters escape to six bytes each, the most a quote can grow.
	_, err := f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=? WHERE message_id=?"), strings.Repeat("\x01", kataevidence.MaxReferences*kataevidence.MaxChars), f.message)
	require.NoError(err)
	evidence := make([]kataevidence.Reference, 0, kataevidence.MaxReferences)
	for i := range kataevidence.MaxReferences {
		evidence = append(evidence, f.evidence(t, i*kataevidence.MaxChars).Reference)
	}
	const issues = 6
	for i := range issues {
		_, err := f.service.Create(t.Context(), fmt.Sprintf("key-large-%d", i), kataissues.CreateInput{Title: "Send the revised budget", PersonID: new(int64(7)), Evidence: evidence})
		require.NoError(err)
	}
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: f.kata.URL, APIKey: katatest.Token, HTTPClient: f.kata.Client(), DefaultProject: "example"})
	require.NoError(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/issues?status=open", f.kata.URL, f.kata.projectID), nil)
	require.NoError(err)
	request.Header.Set("Authorization", "Bearer "+katatest.Token)
	response, err := f.kata.Client().Do(request)
	require.NoError(err)
	listed, err := io.ReadAll(response.Body)
	require.NoError(err)
	require.NoError(response.Body.Close())
	require.Greater(len(listed), int(taskclient.DefaultMaxResponseSize), "the issues outgrow the default response bound")
	agenda, err := personagenda.Service{Tasks: client, People: people{7: {"person-uid-7"}}, Project: "example"}.List(t.Context(), 7)
	require.NoError(err)
	require.Len(agenda.Items, issues)
}

func TestLinkReportsAConflictOtherThanItsGuard(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	created, err := f.service.Create(t.Context(), "key-refused", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{f.evidence(t, 0).Reference}})
	require.NoError(err)
	f.kata.refuseMetadata.Store(true)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{f.evidence(t, 1000).Reference})
	require.ErrorIs(err, taskclient.ErrRequestRejected)
	require.ErrorContains(err, "federated_read_only")
}

func TestLinkTreatsAResyncedPassageAsTheSamePassage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	first := f.evidence(t, 0)
	created, err := f.service.Create(t.Context(), "key-relink", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{first.Reference}})
	require.NoError(err)
	later := f.evidence(t, first.NextRune)
	_, err = f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{later.Reference})
	require.NoError(err)

	// A file re-extracted with the same text, or given a new attachment row by
	// message repair, is the same passage; its occurrence key names it.
	service := f.connect(t)
	service.Evidence = fixedText("Please send the revised budget by Friday.")
	archive, err := f.store.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	original := chunkReference(archive, f.message, "extraction-old", "b")
	chunked, err := service.Create(t.Context(), "key-chunk", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{original}})
	require.NoError(err)
	repaired := original
	repaired.AttachmentID = 9
	chunkLinked, err := service.Link(t.Context(), chunked.Issue.QualifiedRef, []kataevidence.Reference{chunkReference(archive, f.message, "extraction-new", "c"), repaired})
	require.NoError(err)
	assert.Equal(1, passagesOf(t, chunkLinked))
	assert.Empty(commentsOf(t, f.kata, chunked.Issue.UID))

	// New words in the same range are a new passage.
	_, err = f.store.Store.DB().Exec(f.store.Store.Rebind("UPDATE message_bodies SET body_text=? WHERE message_id=?"), strings.Repeat("Earlier context. ", 60)+strings.Repeat("Changed words here. ", 100), f.message)
	require.NoError(err)
	reworded := f.evidence(t, first.NextRune)
	require.NotEqual(later.Excerpt, reworded.Excerpt)
	linked, err := f.service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{reworded.Reference})
	require.NoError(err)
	assert.Equal(3, passagesOf(t, linked))
	comments := commentsOf(t, f.kata, created.Issue.UID)
	require.Len(comments, 2)
	assert.Contains(comments[1], "> "+strings.SplitN(reworded.Excerpt, "\n", 2)[0])

	// A pending quote edited in Kata no longer matches its passage, so the
	// issue's evidence is refused rather than posted.
	edits, err := f.service.Create(t.Context(), "key-edited", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{f.evidence(t, 0).Reference}})
	require.NoError(err)
	opening := f.evidence(t, 500)
	f.kata.failComment.Store(true)
	_, err = f.service.Link(t.Context(), edits.Issue.QualifiedRef, []kataevidence.Reference{opening.Reference})
	require.Error(err)
	stored, err := f.service.Kata.GetTask(t.Context(), katatest.Project, edits.Issue.UID)
	require.NoError(err)
	edited := envelopeOf(t, stored)
	for i := range edited.Entries {
		if edited.Entries[i].Pending != nil {
			edited.Entries[i].Pending.Quote = "Wire the money today."
		}
	}
	_, err = f.service.Kata.MutateMetadataKey(t.Context(), katatest.Project, edits.Issue.UID, kataissues.EvidenceMetadataKey, stored.Metadata[kataissues.EvidenceMetadataKey], edited)
	require.NoError(err)
	_, err = f.service.Link(t.Context(), edits.Issue.QualifiedRef, []kataevidence.Reference{opening.Reference})
	require.ErrorIs(err, kataissues.ErrUnsupportedEvidence)
	assert.Empty(commentsOf(t, f.kata, edits.Issue.UID))
}

func TestLinkAcceptsAnIssueCitingAnotherArchive(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	service := f.connect(t)
	service.Evidence = fixedText("Please send the revised budget by Friday.")
	created, err := service.Create(t.Context(), "key-archives", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{chunkReference("other-archive", 9, "extraction-old", "b")}})
	require.NoError(err)
	archive, err := f.store.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	linked, err := service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{chunkReference(archive, f.message, "extraction-old", "b")})
	require.NoError(err)
	entries := envelopeOf(t, linked).Entries
	require.Len(entries, 2)
	archives := []string{entries[0].Reference.ArchiveUID, entries[1].Reference.ArchiveUID}
	require.ElementsMatch([]string{"other-archive", archive}, archives)
}

func commentsOf(t *testing.T, server *kataServer, uid string) []string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", server.URL, server.projectID, uid), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+katatest.Token)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	var shown katagen.ShowIssueResponseBody
	require.NoError(t, json.UnmarshalRead(response.Body, &shown))
	bodies := make([]string, 0, len(shown.Comments))
	for _, comment := range shown.Comments {
		bodies = append(bodies, comment.Body)
	}
	return bodies
}

// passagesOf counts the distinct passages an issue quotes.
func passagesOf(t *testing.T, issue taskclient.KataTask) int {
	t.Helper()
	passages := map[string]bool{}
	for _, entry := range envelopeOf(t, issue).Entries {
		passages[entry.Passage] = true
	}
	return len(passages)
}

func envelopeOf(t *testing.T, issue taskclient.KataTask) kataissues.Envelope {
	t.Helper()
	data, err := json.Marshal(issue.Metadata[kataissues.EvidenceMetadataKey])
	require.NoError(t, err)
	var envelope kataissues.Envelope
	require.NoError(t, json.Unmarshal(data, &envelope))
	return envelope
}

// pendingOf counts the entries still owed a comment.
func pendingOf(t *testing.T, issue taskclient.KataTask) int {
	t.Helper()
	pending := 0
	for _, entry := range envelopeOf(t, issue).Entries {
		if entry.Pending != nil {
			pending++
		}
	}
	return pending
}

// An issue is found by every message and file it cites.
func TestCitingFindsEveryCitedSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	service := f.connect(t)
	service.Evidence = fixedText("Please send the revised budget by Friday.")
	archive, err := f.store.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	message := f.evidence(t, 0).Reference
	chunk := chunkReference(archive, f.message+1, "extraction-old", "b")
	created, err := service.Create(t.Context(), "key-citing", kataissues.CreateInput{Title: "Send the revised budget", Evidence: []kataevidence.Reference{message}})
	require.NoError(err)
	_, err = service.Link(t.Context(), created.Issue.QualifiedRef, []kataevidence.Reference{chunk})
	require.NoError(err)

	chunkKeys := kataevidence.SourceKeys(chunk)
	for _, key := range []string{kataevidence.SourceKeys(message)[0], chunkKeys[0], chunkKeys[1]} {
		issues, truncated, err := service.Citing(t.Context(), key, 10)
		require.NoError(err)
		require.Len(issues, 1)
		assert.Equal(created.Issue.UID, issues[0].UID)
		assert.False(truncated)
	}

	second, err := service.Create(t.Context(), "key-citing-second", kataissues.CreateInput{Title: "Review the revised budget", Evidence: []kataevidence.Reference{message}})
	require.NoError(err)
	for _, limit := range []int{1, 2} {
		issues, truncated, err := service.Citing(t.Context(), kataevidence.SourceKeys(message)[0], limit)
		require.NoError(err)
		require.Len(issues, limit)
		assert.Equal(created.Issue.UID, issues[0].UID)
		assert.Equal(limit == 1, truncated)
		if limit == 2 {
			assert.Equal(second.Issue.UID, issues[1].UID)
		}
	}
}
