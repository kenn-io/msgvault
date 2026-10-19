package loops

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
)

func TestFailedDetailRefreshPreservesNewerCache(t *testing.T) {
	s, _, now, _ := serviceFixture(t)
	k := wireLoopKata(t, s)
	_, err := s.Track(t.Context(), 1, TrackRequest{})
	require.NoError(t, err)
	link, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	oldChecked := now.Add(-time.Hour)
	link.CheckedAt = &oldChecked
	require.NoError(t, s.st.UpdateLoopKataLinkContext(t.Context(), link))
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	original := k.Service.Get
	k.get = func(ctx context.Context, ref string) (taskclient.KataTask, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return taskclient.KataTask{}, errors.New("remote unavailable")
		}
		issue, err := original(ctx, ref)
		issue.Title = "Newer cache title"
		issue.Status = "closed"
		issue.WebURL = "https://example.test/issues/newer"
		return issue, err
	}
	type detailResult struct {
		response DetailResponse
		err      error
	}
	done := make(chan detailResult, 1)
	go func() {
		response, err := s.Get(t.Context(), 1)
		done <- detailResult{response: response, err: err}
	}()
	<-started
	newer, refreshErr := s.Get(t.Context(), 1)
	close(release)
	failed := <-done
	require.NoError(t, refreshErr)
	require.NoError(t, failed.err)
	require.NotNil(t, newer.Loop.Kata)
	assert.True(t, newer.Loop.Kata.Live)
	got, err := s.st.GetLoopKataLinkContext(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, "Newer cache title", got.IssueTitle)
	assert.Equal(t, "closed", got.IssueStatus)
	assert.Equal(t, "https://example.test/issues/newer", got.IssueWebURL)
	require.NotNil(t, got.CheckedAt)
	assert.True(t, now.Equal(*got.CheckedAt))
	assert.NotEmpty(t, got.LastError)
	require.NotNil(t, failed.response.Loop.Kata)
	assert.Equal(t, "Newer cache title", failed.response.Loop.Kata.Title)
	assert.False(t, failed.response.Loop.Kata.Live)
}

// Overrides model failures at the external boundary; all evidence and issue
// creation otherwise execute against the real archive and embedded Kata.
type loopKataClient struct {
	*kataissues.Service

	evidence *kataevidence.Service
	get      func(context.Context, string) (taskclient.KataTask, error)
	create   func(context.Context, string, kataissues.CreateInput) (kataissues.Result, error)
	comment  func(context.Context, string, string, string) error
}

func (k *loopKataClient) Prepare(ctx context.Context, s []kataevidence.Selector) ([]kataevidence.Evidence, error) {
	return k.evidence.Prepare(ctx, s)
}
func (k *loopKataClient) Get(ctx context.Context, r string) (taskclient.KataTask, error) {
	if k.get != nil {
		return k.get(ctx, r)
	}
	return k.Service.Get(ctx, r)
}
func (k *loopKataClient) Create(ctx context.Context, key string, in kataissues.CreateInput) (kataissues.Result, error) {
	if k.create != nil {
		return k.create(ctx, key, in)
	}
	return k.Service.Create(ctx, key, in)
}
func (k *loopKataClient) Comment(ctx context.Context, uid, key, body string) error {
	if k.comment != nil {
		return k.comment(ctx, uid, key, body)
	}
	return k.Service.Comment(ctx, uid, key, body)
}
func wireLoopKata(t *testing.T, s *Service) *loopKataClient {
	t.Helper()
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project, HTTPClient: server.Client()})
	require.NoError(t, err)
	evidence := kataevidence.New(s.st)
	k := &loopKataClient{Service: &kataissues.Service{Archive: s.st, Kata: client, Evidence: evidence, People: s.st, Project: katatest.Project}, evidence: evidence}
	s.kata = k
	rows, err := s.st.ListOpenLoopsContext(t.Context(), store.LoopListFilter{Status: "all", Limit: 200, Now: s.now()})
	require.NoError(t, err)
	for _, row := range rows {
		_, err = s.st.DB().Exec(s.st.Rebind("INSERT INTO message_bodies(message_id,body_text) VALUES(?,?)"), *row.AnchorMessageID, "Please send the revised budget. Thank you.")
		require.NoError(t, err)
	}
	return k
}

func TestTrackCreatesEvidenceAndReplaysWithoutKata(t *testing.T) {
	s, _, _, person := serviceFixture(t)
	k := wireLoopKata(t, s)
	result, err := s.Track(t.Context(), 1, TrackRequest{})
	require.NoError(t, err)
	assert.False(t, result.Replayed)
	assert.NotEmpty(t, result.Issue.Revision)
	require.NotNil(t, result.Loop.Kata)
	assert.Equal(t, "created", result.Loop.Kata.LinkedVia)
	assert.Equal(t, "created", result.Loop.AgendaState)
	issue, err := k.Get(t.Context(), result.Issue.UID)
	require.NoError(t, err)
	uids, err := s.st.ListPersonUIDsContext(t.Context(), person)
	require.NoError(t, err)
	assert.Contains(t, uids, issue.Metadata[personagenda.PersonMetadataKey])
	assert.Contains(t, issue.Body, "Please send the revised budget.")
	k.get = func(context.Context, string) (taskclient.KataTask, error) {
		return taskclient.KataTask{}, errors.New("must use cache")
	}
	k.create = func(context.Context, string, kataissues.CreateInput) (kataissues.Result, error) {
		return kataissues.Result{}, errors.New("must not create again")
	}
	replay, err := s.Track(t.Context(), 1, TrackRequest{Title: "Changed title"})
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, result.Issue.UID, replay.Issue.UID)
	assert.Empty(t, replay.Issue.Revision)
	list, err := s.List(t.Context(), ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, result.Issue.UID, list.Items[0].Kata.IssueUID)
	detail, err := s.Get(t.Context(), 1)
	require.NoError(t, err)
	assert.False(t, detail.Loop.Kata.Live)
	assert.NotEmpty(t, detail.Loop.Kata.LastError)
	require.NoError(t, s.Untrack(t.Context(), 1))
	require.ErrorIs(t, s.Untrack(t.Context(), 1), store.ErrLoopNotFound)
}
func TestTrackValidationAndExistingIssueOwnership(t *testing.T) {
	s, _, _, _ := serviceFixture(t) //nolint:dogsled // this case needs only the service
	_, err := s.Track(t.Context(), 1, TrackRequest{})
	require.ErrorIs(t, err, ErrKataUnavailable)
	k := wireLoopKata(t, s)
	for _, req := range []TrackRequest{{IssueRef: "example#ab12", Title: "title"}, {IssueRef: "example#ab12", List: "agenda"}, {Title: strings.Repeat("a", 513)}} {
		_, err = s.Track(t.Context(), 1, req)
		require.ErrorIs(t, err, kataissues.ErrInvalidRequest)
	}
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	refs, _, err := PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{FallbackToWindow: true})
	require.NoError(t, err)
	created, err := k.Create(t.Context(), "existing", kataissues.CreateInput{Title: "Existing issue", Evidence: refs})
	require.NoError(t, err)
	result, err := s.Track(t.Context(), 1, TrackRequest{IssueRef: created.Issue.QualifiedRef})
	require.NoError(t, err)
	assert.Equal(t, "existing", result.Loop.Kata.LinkedVia)
	assert.NotEqual(t, "created", result.Loop.AgendaState)
	issue, err := k.Get(t.Context(), created.Issue.UID)
	require.NoError(t, err)
	assert.Empty(t, issue.Metadata[personagenda.PersonMetadataKey])
	replay, err := s.Track(t.Context(), 1, TrackRequest{IssueRef: created.Issue.QualifiedRef})
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	other, err := k.Create(t.Context(), "other", kataissues.CreateInput{Title: "Other issue", Evidence: refs})
	require.NoError(t, err)
	_, err = s.Track(t.Context(), 1, TrackRequest{IssueRef: other.Issue.QualifiedRef})
	require.ErrorIs(t, err, ErrLoopAlreadyTracked)
	_, err = s.Close(t.Context(), 2, "")
	require.NoError(t, err)
	_, err = s.Track(t.Context(), 2, TrackRequest{})
	require.ErrorIs(t, err, store.ErrLoopAlreadyClosed)
	_, err = s.st.DB().Exec("UPDATE open_loops SET counterparty_participant_id=NULL,counterparty_person_id=NULL WHERE id=3")
	require.NoError(t, err)
	_, err = s.Track(t.Context(), 3, TrackRequest{List: "work"})
	require.ErrorIs(t, err, kataissues.ErrInvalidRequest)
}
func TestPrepareLoopEvidenceQuotePolicy(t *testing.T) {
	s, _, _, _ := serviceFixture(t) //nolint:dogsled // this case only needs the service
	k := wireLoopKata(t, s)
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	row.EvidenceJSON = []byte(`[{"quote":"revised budget","quote_start_rune":9999,"quote_end_rune":10000}]`)
	refs, note, err := PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{})
	require.NoError(t, err)
	assert.Empty(t, note)
	require.Len(t, refs, 1)
	resolved, err := k.evidence.ResolveAround(t.Context(), refs[0], 0)
	require.NoError(t, err)
	assert.Equal(t, "revised budget", resolved.Evidence.Excerpt)
	row.EvidenceJSON = []byte(`[{"quote":"a missing quote"}]`)
	_, _, err = PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{})
	require.ErrorIs(t, err, kataevidence.ErrQuoteNotFound)
	refs, note, err = PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{FallbackToWindow: true})
	require.NoError(t, err)
	assert.NotEmpty(t, note)
	assert.Len(t, refs, 1)
	_, err = s.st.DB().Exec(s.st.Rebind("UPDATE message_bodies SET body_text=body_text || ' Please review the budget.' WHERE message_id=?"), *row.AnchorMessageID)
	require.NoError(t, err)
	row.EvidenceJSON = []byte(`[{"quote":"budget"}]`)
	_, _, err = PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{})
	require.ErrorIs(t, err, kataevidence.ErrQuoteAmbiguous)
	refs, note, err = PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{FallbackToWindow: true})
	require.NoError(t, err)
	assert.NotEmpty(t, note)
	assert.Len(t, refs, 1)
	row.AnchorMessageID = nil
	_, _, err = PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{})
	require.ErrorIs(t, err, kataevidence.ErrUnavailable)
}
func TestTrackRecoveryAndDetailRefresh(t *testing.T) {
	s, _, now, _ := serviceFixture(t)
	k := wireLoopKata(t, s)
	var key string
	original := k.Service.Create
	k.create = func(ctx context.Context, inKey string, in kataissues.CreateInput) (kataissues.Result, error) {
		key = inKey
		r, e := original(ctx, inKey, in)
		if e != nil {
			return r, e
		}
		return r, kataissues.ErrIdempotencyConflict
	}
	tracked, err := s.Track(t.Context(), 1, TrackRequest{})
	require.NoError(t, err)
	assert.Equal(t, "loop:1", key)
	assert.NotEmpty(t, tracked.Note)
	s.now = func() time.Time { return now.Add(time.Hour) }
	detail, err := s.Get(t.Context(), 1)
	require.NoError(t, err)
	assert.True(t, detail.Loop.Kata.Live)
	assert.Equal(t, now.Add(time.Hour), *detail.Loop.Kata.CheckedAt)
	k.create = func(context.Context, string, kataissues.CreateInput) (kataissues.Result, error) {
		return kataissues.Result{}, kataissues.ErrIssueDeleted
	}
	_, err = s.Track(t.Context(), 2, TrackRequest{})
	require.ErrorIs(t, err, kataissues.ErrIssueDeleted)
}

func TestTrackAllowsClosureDuringCreateAndRejectsMissingEvidence(t *testing.T) {
	s, _, now, _ := serviceFixture(t)
	k := wireLoopKata(t, s)
	original := k.Service.Create
	k.create = func(ctx context.Context, key string, in kataissues.CreateInput) (kataissues.Result, error) {
		r, err := original(ctx, key, in)
		if err == nil {
			_, err = s.st.CloseLoopContext(ctx, 1, "handled", now.Add(time.Second))
		}
		return r, err
	}
	result, err := s.Track(t.Context(), 1, TrackRequest{})
	require.NoError(t, err)
	assert.Equal(t, "closed", result.Loop.Status)
	require.NotNil(t, result.Loop.Kata)
	_, err = s.st.DB().Exec("UPDATE open_loops SET anchor_message_id=NULL WHERE id=2")
	require.NoError(t, err)
	_, err = s.Track(t.Context(), 2, TrackRequest{})
	require.ErrorIs(t, err, kataevidence.ErrUnavailable)
	row, err := s.st.GetOpenLoopContext(t.Context(), 3)
	require.NoError(t, err)
	_, err = s.st.DB().Exec(s.st.Rebind("UPDATE messages SET source_message_id=NULL WHERE id=?"), *row.AnchorMessageID)
	require.NoError(t, err)
	_, err = s.Track(t.Context(), 3, TrackRequest{})
	require.ErrorIs(t, err, kataevidence.ErrUnsupported)
}

func TestTrackConcurrentRequestsKeepOneLink(t *testing.T) {
	s, _, _, _ := serviceFixture(t) //nolint:dogsled // this case only needs the service
	wireLoopKata(t, s)
	results := make(chan TrackResponse, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() { r, e := s.Track(t.Context(), 1, TrackRequest{}); results <- r; errs <- e }()
	}
	first, second := <-results, <-results
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	assert.Equal(t, first.Issue.UID, second.Issue.UID)
	var count int
	require.NoError(t, s.st.DB().QueryRow("SELECT COUNT(*) FROM loop_kata_links WHERE loop_id=1").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestTrackCitesAnchorAndLatestNudge(t *testing.T) {
	s, f, _, _ := serviceFixture(t)
	k := wireLoopKata(t, s)
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	latest := f.CreateMessage("latest-nudge")
	_, err = s.st.DB().Exec(s.st.Rebind("UPDATE messages SET conversation_id=? WHERE id=?"), row.ConversationID, latest)
	require.NoError(t, err)
	_, err = s.st.DB().Exec(s.st.Rebind("INSERT INTO message_bodies(message_id,body_text) VALUES(?,?)"), latest, "Following up on the earlier budget request.")
	require.NoError(t, err)
	_, err = s.st.DB().Exec(s.st.Rebind("UPDATE open_loops SET last_activity_message_id=? WHERE id=1"), latest)
	require.NoError(t, err)
	result, err := s.Track(t.Context(), 1, TrackRequest{})
	require.NoError(t, err)
	issue, err := k.Get(t.Context(), result.Issue.UID)
	require.NoError(t, err)
	assert.Contains(t, issue.Body, "Please send the revised budget.")
	assert.Contains(t, issue.Body, "Following up on the earlier budget request.")
}
func TestTrackExistingIssueDoesNotMoveAnotherPersonsAgenda(t *testing.T) {
	s, f, _, personID := serviceFixture(t)
	k := wireLoopKata(t, s)
	otherParticipant := f.EnsureParticipant("another@example.test", "Another Person", "example.test")
	other, _, err := s.st.CreatePersonFromParticipant(otherParticipant)
	require.NoError(t, err)
	row, err := s.st.GetOpenLoopContext(t.Context(), 1)
	require.NoError(t, err)
	refs, _, err := PrepareLoopEvidence(t.Context(), k, row, EvidencePolicy{})
	require.NoError(t, err)
	for _, tc := range []struct {
		id, person  int64
		wantCreated bool
	}{{1, personID, true}, {2, other.ID, false}} {
		issue, err := k.Create(t.Context(), fmt.Sprintf("owned-agenda-%d", tc.id), kataissues.CreateInput{Title: "Existing agenda item", PersonID: &tc.person, List: "work", Evidence: refs})
		require.NoError(t, err)
		beforePerson := issue.Issue.Metadata[personagenda.PersonMetadataKey]
		result, err := s.Track(t.Context(), tc.id, TrackRequest{IssueRef: issue.Issue.QualifiedRef})
		require.NoError(t, err)
		assert.Equal(t, tc.wantCreated, result.Loop.AgendaState == "created")
		after, err := k.Get(t.Context(), issue.Issue.UID)
		require.NoError(t, err)
		assert.Equal(t, beforePerson, after.Metadata[personagenda.PersonMetadataKey])
		assert.Equal(t, "work", after.Metadata[personagenda.ListMetadataKey])
	}
}
func TestDetailRefreshCannotWriteThroughSameIssueRelink(t *testing.T) {
	s, k, _, _, _ := bridgeFixture(t) //nolint:dogsled // this case only needs the archive store and Kata client
	original := k.Service.Get
	k.get = func(ctx context.Context, r string) (taskclient.KataTask, error) {
		issue, err := original(ctx, r)
		if err != nil {
			return issue, err
		}
		link, err := s.st.GetLoopKataLinkContext(ctx, 1)
		require.NoError(t, err)
		require.NoError(t, s.Untrack(ctx, 1))
		link.IssueTitle = "Replacement cache"
		_, _, err = s.st.InsertLoopKataLinkContext(ctx, link, "")
		require.NoError(t, err)
		issue.Title = "Stale external title"
		return issue, nil
	}
	detail, err := s.Get(t.Context(), 1)
	require.NoError(t, err)
	require.NotNil(t, detail.Loop.Kata)
	assert.Equal(t, "Replacement cache", detail.Loop.Kata.Title)
	assert.False(t, detail.Loop.Kata.Live)
}
