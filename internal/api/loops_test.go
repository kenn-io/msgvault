package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type loopFixture struct {
	f      *storetest.Fixture
	router http.Handler
	owed   int64
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

func TestCommitmentDecisionRejectsNumericAndInvalidIDs(t *testing.T) {
	a := assert.New(t)
	fixture := newLoopFixture(t)
	for _, tc := range []struct {
		id, request string
		status      int
	}{
		{"12", `{"state":"done"}`, http.StatusBadRequest},
		{"c012", `{"state":"done"}`, http.StatusBadRequest},
		{"c12", `{"state":"snoozed"}`, http.StatusBadRequest},
		{"c12", `{"state":"done"}`, http.StatusNotFound},
	} {
		response := fixture.do(t, http.MethodPost, "/api/v1/loops/commitments/"+tc.id+"/decision", tc.request)
		a.Equal(tc.status, response.Code, response.Body.String())
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
