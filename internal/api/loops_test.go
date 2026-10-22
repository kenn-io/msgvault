package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
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
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := storetest.NewLoopOriginalReadStore(f, loops.MaxListEvidenceComputations+1, time.Now().UTC())
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: st, Scheduler: newMockScheduler(), Logger: testLogger()})
	w := httptest.NewRecorder()
	server.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/loops", nil))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var response LoopListResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	require.True(response.CoverageIncomplete)
	require.Len(response.Items, 1)
	assert.Equal(int64(1), response.Items[0].AnchorMessageID)
	assert.Equal("Can you send the contract?", response.Items[0].Summary)
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
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/loops", nil))
	assert.Equal(http.StatusServiceUnavailable, w.Code)
}

func TestDelegatedLoopRoutesRequireOwner(t *testing.T) {
	l := newLoopFixture(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Server.APIKey = "test-owner-key"
	cfg.Server.AgentAccess = true
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: l.f.Store, Scheduler: newMockScheduler(), Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("test-reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: l.f.Source.ID, Type: l.f.Source.SourceType, Identifier: l.f.Source.Identifier}}, time.Time{})
	require.NoError(t, err)
	router := srv.Router()
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", l.owed), "", http.StatusOK},
		{http.MethodGet, "/api/v1/loops", "", http.StatusUnauthorized},
		{http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/done", l.owed), "", http.StatusUnauthorized},
		{http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/snooze", l.owed), fmt.Sprintf(`{"until":%q}`, time.Now().Add(48*time.Hour).Format(time.RFC3339)), http.StatusUnauthorized},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusUnauthorized {
				var denied struct {
					Error string `json:"error"`
				}
				require.NoError(json.Unmarshal(w.Body.Bytes(), &denied))
				assert.Equal("unauthorized", denied.Error)
			}
			if tc.status == http.StatusOK {
				var message map[string]any
				require.NoError(json.Unmarshal(w.Body.Bytes(), &message))
				assert.NotEmpty(message)
			}
		})
	}
}
