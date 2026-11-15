package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type loopFixture struct {
	f      *storetest.Fixture
	router http.Handler
	owed   int64
}

type deadlineLoopContext struct {
	context.Context

	done chan struct{}
}

func (c *deadlineLoopContext) Done() <-chan struct{} { return c.done }
func (c *deadlineLoopContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type deadlineLoopStore struct {
	*store.Store

	deadline *deadlineLoopContext
	stage    string
}

func (s *deadlineLoopStore) HasActivePersonCommitmentConsent(context.Context, string) (bool, error) {
	if s.stage == "error" {
		return false, errors.New("consent read failed")
	}
	if s.stage == "consent" {
		close(s.deadline.done)
		return false, context.DeadlineExceeded
	}
	return true, nil
}

func (s *deadlineLoopStore) PersonCommitmentDiscoveryContext(context.Context, int64, string) (*time.Time, bool, error) {
	close(s.deadline.done)
	return nil, false, context.DeadlineExceeded
}

func newLoopFixture(t *testing.T) *loopFixture {
	t.Helper()
	f := storetest.New(t)
	now := time.Now().UTC()
	const owner, alex, sam = storetest.OwnerAddress, "alex@example.test", "sam@example.test"
	f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: owner, To: []string{alex}, Body: "Hello", SentAt: now.Add(-10 * 24 * time.Hour)})
	owed := f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: alex, To: []string{owner}, Subject: "Contract draft", Body: "Can you send the contract?", SentAt: now.Add(-5 * 24 * time.Hour)})
	f.PersistEmail(storetest.EmailSpec{Thread: "asked", From: owner, To: []string{sam}, Subject: "Lunch Friday?", Body: "Are you free?", SentAt: now.Add(-20 * 24 * time.Hour)})
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, Scheduler: newMockScheduler(), Logger: testLogger()})
	return &loopFixture{f: f, router: server.Router(), owed: owed}
}

func (l *loopFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	l.router.ServeHTTP(w, req)
	return w
}

func (l *loopFixture) list(t *testing.T, query string) LoopListResponse {
	t.Helper()
	w := l.do(t, http.MethodGet, "/api/v1/loops"+query, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var out LoopListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func TestCommitmentDecisionMapsMalformedAndUnknownIDs(t *testing.T) {
	a := assert.New(t)
	fixture := newLoopFixture(t)
	fixture.router = NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: &conflictingCommitmentStore{fixture.f.Store}, Scheduler: newMockScheduler(), Logger: testLogger()}).Router()
	for _, tc := range []struct {
		id, request string
		status      int
	}{
		{"12", `{"state":"done"}`, http.StatusBadRequest},
		{"c012", `{"state":"done"}`, http.StatusBadRequest},
		{"c12", `{"state":"done"}`, http.StatusNotFound},
		{"c1", `{"state":"done"}`, http.StatusConflict},
	} {
		response := fixture.do(t, http.MethodPost, "/api/v1/loops/commitments/"+tc.id+"/decision", tc.request)
		a.Equal(tc.status, response.Code, response.Body.String())
		if tc.status == http.StatusConflict {
			a.Contains(response.Body.String(), "commitment_changed")
		}
	}
}

func TestListLoopsFilters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	l := newLoopFixture(t)
	alex := l.f.EnsureParticipant("alex@example.test", "", "example.test")
	person, _, err := l.f.Store.CreatePersonFromParticipant(alex)
	require.NoError(err)

	out := l.list(t, "")
	require.Len(out.Items, 2)
	assert.False(out.Truncated)

	byPerson := l.list(t, fmt.Sprintf("?person_id=%d", person.ID))
	require.Len(byPerson.Items, 1)
	assert.Equal(l.owed, byPerson.Items[0].AnchorMessageID)
	assert.Equal("email", byPerson.Items[0].Channel)
	assert.Equal("Contract draft", byPerson.Items[0].Summary)
	assert.Equal("alex@example.test", byPerson.Items[0].Counterparty.Address)

	truncated := l.list(t, "?limit=1")
	assert.Len(truncated.Items, 1)
	assert.True(truncated.Truncated)
}

func TestListLoopsReportsCommitmentDiscoveryWhenEnabled(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.People.Sweep.Enabled = true
	cfg.People.Sweep.Commitments.Enabled = true
	provider := completeAPIProvider("EXAMPLE_KEY", "example-model")
	provider.SourceUntil = "2025-06-30"
	provider.AllowSensitive = true
	provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
	configureAPIProvider(cfg, provider)
	cfg.People.Sweep.ApplyDefaults()
	for _, stage := range []string{"consent", "discovery", "error"} {
		t.Run(stage, func(t *testing.T) {
			a, r := assert.New(t), require.New(t)
			fixture := newLoopFixture(t)
			ctx := &deadlineLoopContext{Context: t.Context(), done: make(chan struct{})}
			st := &deadlineLoopStore{Store: fixture.f.Store, deadline: ctx, stage: stage}
			server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Scheduler: newMockScheduler(), Logger: testLogger()})
			w := httptest.NewRecorder()
			server.Router().ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/loops", nil))
			if stage == "error" {
				a.Equal(http.StatusInternalServerError, w.Code)
				return
			}
			r.Equal(http.StatusOK, w.Code, w.Body.String())
			var response LoopListResponse
			r.NoError(json.Unmarshal(w.Body.Bytes(), &response))
			a.True(response.CoverageIncomplete)
			a.Len(response.Items, 2)
			if stage == "discovery" {
				r.NotNil(response.CommitmentsComplete)
				a.False(*response.CommitmentsComplete)
			}
		})
	}
	profile, err := cfg.People.Sweep.Profile()
	requirements.NoError(err)
	_, err = f.Store.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)
	requirements.NoError(f.Store.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(), DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model}))
	participant := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	requirements.NoError(err)
	server := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Scheduler: newMockScheduler(), Logger: testLogger()})
	read := func() LoopListResponse {
		w := httptest.NewRecorder()
		server.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/loops?person_id=%d", person.ID), nil))
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		var response LoopListResponse
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
		return response
	}
	response := read()
	requirements.NotNil(response.CommitmentsComplete)
	assertions.False(*response.CommitmentsComplete)
	assertions.Empty(response.Commitments)
	_, _, err = f.Store.GrantPersonCommitmentConsent(t.Context(), profile.Fingerprint, "test")
	requirements.NoError(err)
	response = read()
	requirements.NotNil(response.CommitmentsComplete)
	assertions.False(*response.CommitmentsComplete)
	requirements.NotNil(response.CommitmentsUntil)
	assertions.Equal("2025-06-30", response.CommitmentsUntil.Format(time.DateOnly))
	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_commitment_frontiers (person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint, discovery_since) VALUES (?, 0, ?, -1, ?, ?)`), person.ID, since, peoplesweep.CommitmentScopeFingerprint(profile, cfg.People.Sweep.Commitments), since)
	requirements.NoError(err)
	response = read()
	requirements.NotNil(response.CommitmentsSince)
	assertions.True(response.CommitmentsSince.Equal(since))
	_, err = f.Store.RevokePersonCommitmentConsent(t.Context(), profile.Fingerprint, "test")
	requirements.NoError(err)
	response = read()
	requirements.NotNil(response.CommitmentsComplete)
	assertions.False(*response.CommitmentsComplete)
}

func TestListLoopsReportsIncompleteCoverage(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	storetest.PersistLoopOriginalReadFixture(f, loops.MaxListEvidenceComputations+1, time.Now().UTC())
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, Scheduler: newMockScheduler(), Logger: testLogger()})
	w := httptest.NewRecorder()
	server.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/loops", nil))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var response LoopListResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	require.True(response.CoverageIncomplete)
}

func TestListLoopsAnswersDuringBackupFreezeWithoutWritingEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	l := newLoopFixture(t)
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:         l.f.Store,
		Scheduler:     newMockScheduler(),
		Logger:        testLogger(),
		OperationGate: NewSerialOperationGate(),
	})
	evidence := func() int {
		var count int
		require.NoError(l.f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM loop_message_evidence`).Scan(&count))
		return count
	}
	list := func(method string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/loops", nil)
		request.RemoteAddr = "127.0.0.1:54321"
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		return response
	}

	token := beginBackupFreeze(t, srv)
	var frozen LoopListResponse
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := list(method)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		if method == http.MethodGet {
			require.NoError(json.Unmarshal(response.Body.Bytes(), &frozen))
		}
		assert.Zero(evidence(), method+" must not write evidence while the backup freeze holds the gate")
	}
	require.NotEmpty(frozen.Items, "a frozen archive still answers from its messages")

	ended := httptest.NewRecorder()
	srv.Router().ServeHTTP(ended, endBackupFreeze(t, token))
	require.Equal(http.StatusOK, ended.Code, ended.Body.String())
	response := list(http.MethodGet)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var thawed LoopListResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &thawed))
	assert.Equal(frozen.Items, thawed.Items, "skipping evidence writes does not change the answer")
	assert.Positive(evidence(), "the list stores evidence after the backup freeze ends")
}

func TestListLoopsRejectsBadQuery(t *testing.T) {
	l := newLoopFixture(t)
	for _, query := range []string{"?limit=0", "?limit=201", "?person_id=0", "?person_id=x", "?limit=1&limit=2", "?person_id=%ZZ", "?limit=%ZZ"} {
		t.Run(query, func(t *testing.T) {
			assert := assert.New(t)
			assert.Equal(http.StatusBadRequest, l.do(t, http.MethodGet, "/api/v1/loops"+query, "").Code)
		})
	}
}

func TestLoopSnoozeReturnsDecisionAndNoStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	l := newLoopFixture(t)

	w := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/snooze", l.owed), fmt.Sprintf(`{"until":%q}`, time.Now().Add(48*time.Hour).Format(time.RFC3339)))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	assert.Equal("no-store", w.Header().Get("Cache-Control"))
	var decision LoopDecisionResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &decision))
	assert.Equal("snoozed", decision.State)
}

func TestLoopActionsRejectNonAnchorsAndBadInput(t *testing.T) {
	assert := assert.New(t)
	l := newLoopFixture(t)
	for _, tt := range []struct {
		name, path, body string
		want             int
	}{
		{"non-numeric id", "/api/v1/loops/abc/done", "", http.StatusBadRequest},
		{"snooze in the past", fmt.Sprintf("/api/v1/loops/%d/snooze", l.owed), `{"until":"2020-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{"snooze without until", fmt.Sprintf("/api/v1/loops/%d/snooze", l.owed), `{}`, http.StatusBadRequest},
		{"snooze with unknown field", fmt.Sprintf("/api/v1/loops/%d/snooze", l.owed), `{"until":"2999-01-01T00:00:00Z","x":1}`, http.StatusBadRequest},
		{"zero question", fmt.Sprintf("/api/v1/loops/%d/done?question_message_ids=0", l.owed), "", http.StatusBadRequest},
		{"non-numeric question", fmt.Sprintf("/api/v1/loops/%d/done?question_message_ids=abc", l.owed), "", http.StatusBadRequest},
		{"question outside the loop", fmt.Sprintf("/api/v1/loops/%d/done?question_message_ids=%d,%d", l.owed, l.owed, l.owed-1), "", http.StatusNotFound},
		{"anchor left out of the listed questions", fmt.Sprintf("/api/v1/loops/%d/done?question_message_ids=%d", l.owed, l.owed+1), "", http.StatusBadRequest},
		{"reopen ignores question_message_ids", fmt.Sprintf("/api/v1/loops/%d/reopen?question_message_ids=abc", l.owed), "", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(tt.want, l.do(t, http.MethodPost, tt.path, tt.body).Code)
		})
	}

	w := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/done", l.owed-1), "")
	assert.Equal(http.StatusNotFound, w.Code, "a message that anchors nothing")
	assert.Contains(w.Body.String(), "loop_not_found")
}

func TestLoopRoutesUnavailableWithoutStoreCapability(t *testing.T) {
	assert := assert.New(t)
	srv := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: &mockStore{}, Scheduler: newMockScheduler(), Logger: testLogger()})
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/loops", nil))
	assert.Equal(http.StatusServiceUnavailable, w.Code)
}

func TestLoopActionsRetryAfterIncompleteEvidence(t *testing.T) {
	for _, action := range []string{"done", "snooze"} {
		t.Run(action, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			now := time.Now().UTC()
			storetest.PersistLoopOriginalReadFixture(f, loops.MaxListEvidenceComputations+1, now)
			var anchor, conversationID int64
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT id, conversation_id FROM messages WHERE subject = 'Can you send the contract?'`).Scan(&anchor, &conversationID))
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET conversation_id = ?`), conversationID)
			require.NoError(err)
			server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, Scheduler: newMockScheduler(), Logger: testLogger()})
			fixture := &loopFixture{f: f, router: server.Router()}
			body := ""
			if action == "snooze" {
				body = fmt.Sprintf(`{"until":%q}`, now.Add(24*time.Hour).Format(time.RFC3339))
			}
			path := fmt.Sprintf("/api/v1/loops/%d/%s", anchor, action)
			first := fixture.do(t, http.MethodPost, path, body)
			assert.Equal(http.StatusServiceUnavailable, first.Code, first.Body.String())
			assert.Equal("1", first.Header().Get("Retry-After"))
			assert.Contains(first.Body.String(), `"loop_scan_incomplete"`)
			second := fixture.do(t, http.MethodPost, path, body)
			require.Equal(http.StatusOK, second.Code, second.Body.String())
		})
	}
}

func TestLoopActionsCoverOnlyTheListedQuestions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	l := newLoopFixture(t)
	now := time.Now().UTC()
	ask := func(body string, age time.Duration) int64 {
		return l.f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: body, SentAt: now.Add(-age)})
	}
	deck := ask("And the deck?", 2*24*time.Hour)
	var questions []string
	for _, item := range l.list(t, "").Items {
		if item.AnchorMessageID == l.owed {
			assert.Equal([]int64{l.owed, deck}, item.QuestionMessageIDs, "the list names the questions it covers")
			for _, id := range item.QuestionMessageIDs {
				questions = append(questions, strconv.FormatInt(id, 10))
			}
		}
	}
	require.Len(questions, 2)
	backfilled := ask("Did the invoice go out?", 3*24*time.Hour)

	w := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/done?question_message_ids=%s", l.owed, strings.Join(questions, ",")), "")
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var anchors []int64
	for _, item := range l.list(t, "").Items {
		anchors = append(anchors, item.AnchorMessageID)
	}
	assert.Contains(anchors, backfilled, "a question imported between listed ones stays open")
}

func TestLoopReopenReturnsQuestionToList(t *testing.T) {
	for _, tt := range []struct{ action, body string }{
		{"done", ""},
		{"snooze", fmt.Sprintf(`{"until":%q}`, time.Now().Add(48*time.Hour).Format(time.RFC3339))},
	} {
		t.Run(tt.action, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			l := newLoopFixture(t)
			decided := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/%s", l.owed, tt.action), tt.body)
			require.Equal(http.StatusOK, decided.Code, decided.Body.String())
			for _, item := range l.list(t, "").Items {
				require.NotEqual(l.owed, item.AnchorMessageID)
			}

			reopened := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/reopen", l.owed), "")
			require.Equal(http.StatusOK, reopened.Code, reopened.Body.String())
			var anchors []int64
			for _, item := range l.list(t, "").Items {
				anchors = append(anchors, item.AnchorMessageID)
			}
			assert.Contains(anchors, l.owed)
		})
	}
}

func TestLoopReopenLeavesOtherDecisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	l := newLoopFixture(t)
	open := l.list(t, "").Items
	require.Len(open, 2)
	for _, item := range open {
		w := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/done", item.AnchorMessageID), "")
		require.Equal(http.StatusOK, w.Code, w.Body.String())
	}

	w := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/reopen", l.owed), "")
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	listed := l.list(t, "").Items
	require.Len(listed, 1)
	assert.Equal(l.owed, listed[0].AnchorMessageID)
}

func TestLoopReopenWithoutDecisionIsNotFound(t *testing.T) {
	assert := assert.New(t)
	l := newLoopFixture(t)
	w := l.do(t, http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/reopen", l.owed), "")
	assert.Equal(http.StatusNotFound, w.Code)
	assert.Contains(w.Body.String(), `"error":"loop_decision_not_found"`)
}

type pausedLoopStore struct {
	*store.Store

	reads     atomic.Int64
	pauseAt   int64
	paused    chan struct{}
	resume    chan struct{}
	completed chan struct{}
}

func (s *pausedLoopStore) LoopOriginalEvidenceContext(ctx context.Context, id int64) (mime.AutomationSignals, bool, bool, error) {
	n := s.reads.Add(1)
	if n == s.pauseAt {
		close(s.paused)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return mime.AutomationSignals{}, false, false, ctx.Err()
		}
	}
	signals, attachment, unreadable, err := s.Store.LoopOriginalEvidenceContext(ctx, id)
	if n == s.pauseAt && s.completed != nil {
		close(s.completed)
	}
	return signals, attachment, unreadable, err
}

func shutdownLoopServer(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	defer cancel()
	require.NoError(t, s.Shutdown(ctx))
}

func awaitLoopSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for loop worker")
	}
}

func TestIncompleteListsContinueEachQueuedScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	storetest.PersistLoopOriginalReadFixture(f, loops.MaxListEvidenceComputations+1, now)
	st := &pausedLoopStore{Store: f.Store, pauseAt: loops.MaxListEvidenceComputations + 1, paused: make(chan struct{}), resume: make(chan struct{})}
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Scheduler: newMockScheduler(), Logger: testLogger()})
	t.Cleanup(func() { shutdownLoopServer(t, server) })
	list := func(ctx context.Context, path string) LoopListResponse {
		w := httptest.NewRecorder()
		server.Router().ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		var result LoopListResponse
		require.NoError(json.Unmarshal(w.Body.Bytes(), &result))
		return result
	}
	ctx, cancel := context.WithCancel(t.Context())
	require.True(list(ctx, "/api/v1/loops").CoverageIncomplete)
	cancel()
	awaitLoopSignal(t, st.paused)
	var first int64
	for i := range loops.MaxListEvidenceComputations + 1 {
		body := "Thanks!"
		if i == 0 {
			body = "Can you send the contract?"
		}
		id := f.PersistEmail(storetest.EmailSpec{Thread: fmt.Sprintf("older-%d", i), From: "sam@example.test", To: []string{storetest.OwnerAddress}, Body: body, SentAt: now.Add(-30 * 24 * time.Hour)})
		if i == 0 {
			first = id
		}
	}
	participant := f.EnsureParticipant("sam@example.test", "", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	personPath := fmt.Sprintf("/api/v1/loops?person_id=%d", person.ID)
	require.True(list(t.Context(), personPath).CoverageIncomplete)
	close(st.resume)
	require.Eventually(func() bool {
		server.importMu.Lock()
		defer server.importMu.Unlock()
		return !server.loopEvidenceRunning
	}, 20*time.Second, 10*time.Millisecond)
	reads := st.reads.Load()
	personResult := list(t.Context(), personPath)
	assert.False(personResult.CoverageIncomplete)
	require.Len(personResult.Items, 1)
	assert.Equal(first, personResult.Items[0].AnchorMessageID)
	all := list(t.Context(), "/api/v1/loops")
	assert.False(all.CoverageIncomplete)
	assert.Equal(reads, st.reads.Load())
}

func TestLoopBackfillStopsWhenServerShutsDown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	first := f.PersistEmail(storetest.EmailSpec{Thread: "first", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you send it?", SentAt: now.Add(-5 * 24 * time.Hour)})
	second := f.PersistEmail(storetest.EmailSpec{Thread: "second", From: "sam@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you review it?", SentAt: now.Add(-4 * 24 * time.Hour)})
	st := &pausedLoopStore{Store: f.Store, pauseAt: 2, paused: make(chan struct{}), resume: make(chan struct{}), completed: make(chan struct{})}
	gate := NewSerialOperationGate()
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Scheduler: newMockScheduler(), Logger: testLogger(), OperationGate: gate})
	t.Cleanup(func() { shutdownLoopServer(t, server) })
	server.startLoopEvidence(st, 0)
	awaitLoopSignal(t, st.paused)
	release, ok := gate.BeginWorkContext(t.Context())
	require.True(ok)
	close(st.resume)
	awaitLoopSignal(t, st.completed)
	shutdownLoopServer(t, server)
	release()
	reads := st.reads.Load()
	next := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Scheduler: newMockScheduler(), Logger: testLogger()})
	t.Cleanup(func() { shutdownLoopServer(t, next) })
	w := httptest.NewRecorder()
	next.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/loops", nil))
	require.Equal(http.StatusOK, w.Code)
	var result LoopListResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &result))
	require.Len(result.Items, 2)
	assert.Equal(first, result.Items[0].AnchorMessageID)
	assert.Equal(second, result.Items[1].AnchorMessageID)
	assert.Equal(reads+1, st.reads.Load())
}

type conflictingCommitmentStore struct{ *store.Store }

func (s *conflictingCommitmentStore) DecidePersonCommitmentContext(_ context.Context, id int64, _ string, _ *time.Time, _ time.Time) (store.PersonCommitment, error) {
	if id == 1 {
		return store.PersonCommitment{}, store.ErrCommitmentChanged
	}
	return store.PersonCommitment{}, sql.ErrNoRows
}

type pagingCommitmentStore struct {
	*store.Store

	offset        int
	includeClosed bool
}

func (s *pagingCommitmentStore) ListPersonCommitmentsContext(_ context.Context, _ int64, _ time.Time, _, offset int, includeClosed bool) ([]store.PersonCommitment, error) {
	s.offset, s.includeClosed = offset, includeClosed
	return []store.PersonCommitment{{ID: 201, PersonID: 1, TargetKey: "promise-201", AnchorMessageID: 1, Direction: "owed_to", Quote: "I will call.", Start: 0, End: 12, Summary: "Call", Freshness: "fresh", PromisedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), State: "open"}, {ID: 202, PersonID: 1, TargetKey: "promise-202", AnchorMessageID: 2, Direction: "owed_to", Quote: "I will write.", Start: 0, End: 13, Summary: "Write", Freshness: "stale", PromisedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), State: "open"}}, nil
}

func TestListLoopsPagesCommitments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := &pagingCommitmentStore{Store: f.Store}
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Scheduler: newMockScheduler(), Logger: testLogger()})
	w := httptest.NewRecorder()
	server.Router().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/loops?limit=1&commitment_offset=200&include_closed=true", nil))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	assert.Equal(200, st.offset)
	assert.True(st.includeClosed)
	var response LoopListResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	require.NotEmpty(response.Commitments)
	assert.Equal(LoopCommitment{ID: "c201", PersonID: 1, Direction: "owed_to", Summary: "Call", Promise: CommitmentQuote{MessageID: 1, Text: "I will call.", Start: 0, End: 12}, PromisedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Freshness: "fresh", State: "open"}, response.Commitments[0])
}
