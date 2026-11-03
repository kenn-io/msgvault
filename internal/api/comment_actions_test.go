package api

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
)

type commentHTTPStore struct {
	mockStore

	calls  int
	fail   error
	req    meta.ActionRequest
	filter CommentQueueRequest
}

func (s *commentHTTPStore) RunCommentAction(ctx context.Context, id int64, req meta.ActionRequest, gate func(context.Context) (func(), error)) (*meta.ActionResult, error) {
	if !req.DryRun {
		release, err := gate(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	s.calls++
	s.req = req
	return &meta.ActionResult{MessageID: id, DryRun: req.DryRun, Confirm: "binding"}, s.fail
}
func (s *commentHTTPStore) ListCommentActions(context.Context, int64) ([]meta.Receipt, error) {
	s.calls++
	return []meta.Receipt{}, nil
}
func (s *commentHTTPStore) UpdateCommentTriage(ctx context.Context, id int64, state, expect string, gate func(context.Context) (func(), error)) (string, error) {
	release, err := gate(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	s.calls++
	return state, s.fail
}
func (s *commentHTTPStore) ListCommentQueue(_ context.Context, f CommentQueueRequest) (*CommentQueueResponse, error) {
	s.calls++
	s.filter = f
	return &CommentQueueResponse{Items: []CommentQueueItem{{MessageID: 7}}, NextBeforeID: 7}, nil
}
func commentHTTPCall(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:4242"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	return w
}
func TestCommentActionRouteOwnerOnly(t *testing.T) {
	st := &commentHTTPStore{}
	s := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "reader", APIKey: "remote"}}}}, st, nil, testLogger())
	s.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := s.agentGrants.Issue("example", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 1, Type: "facebook_comments", Identifier: "app:9:page:100"}}, time.Time{})
	require.NoError(t, err)
	for _, route := range []struct{ method, path, body string }{{"POST", "/api/v1/messages/7/comment-actions", `{"action":"hide"}`}, {"GET", "/api/v1/messages/7/comment-actions", ""}, {"PUT", "/api/v1/messages/7/comment-triage", `{"state":"handled"}`}, {"GET", "/api/v1/comment-queue", ""}} {
		for _, credential := range []string{secret, "remote"} {
			r := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			r.RemoteAddr = "127.0.0.1:4242"
			r.Header.Set("Content-Type", "application/json")
			if credential == secret {
				r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			} else {
				r.Header.Set("X-Api-Key", credential)
			}
			w := httptest.NewRecorder()
			s.Router().ServeHTTP(w, r)
			assert.Equal(t, 401, w.Code, w.Body.String())
		}
	}
	assert.Zero(t, st.calls)
}
func TestCommentActionRouteDryRun(t *testing.T) { //nolint:paralleltest // changes the gate wait deadline
	assertions := assert.New(t)
	requirements := require.New(t)
	old := operationGateWaitLimit
	operationGateWaitLimit = 10 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = old })
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "sync")
	requirements.True(ok)
	defer release()
	st := &commentHTTPStore{}
	s := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), OperationGate: gate})
	w := commentHTTPCall(s, "POST", "/api/v1/messages/7/comment-actions", `{"action":"delete","dry_run":true}`)
	requirements.Equal(200, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), `"confirm":"binding"`)
	assertions.Equal(503, commentHTTPCall(s, "POST", "/api/v1/messages/7/comment-actions", `{"action":"hide"}`).Code)
	assertions.Equal(503, commentHTTPCall(s, "PUT", "/api/v1/messages/7/comment-triage", `{"state":"handled"}`).Code)
	assertions.Equal(1, st.calls)
}
func TestCommentActionRouteErrorStatuses(t *testing.T) {
	require := require.New(t)
	st := &commentHTTPStore{}
	s := NewServer(&config.Config{}, st, nil, testLogger())
	for status, codes := range map[int]string{400: "invalid_request not_a_comment target_deleted unsupported_source private_reply_not_allowed confirmation_mismatch", 403: "action_not_enabled permission_missing app_access_insufficient token_invalid", 404: "message_not_found", 409: "stale_identity sync_active outcome_unknown triage_conflict", 429: "rate_limited", 500: "remote_accepted_local_failed local_failed", 502: "provider_rejected messaging_window_closed thread_owned_by_other_app recipient_unavailable", 503: "credential_unavailable"} {
		for code := range strings.FieldsSeq(codes) {
			st.fail = &meta.ActionError{Code: code, Message: code, Result: &meta.ActionResult{Receipt: &meta.Receipt{ID: 3, State: "unknown"}}}
			w := commentHTTPCall(s, "POST", "/api/v1/messages/7/comment-actions", `{"action":"hide"}`)
			require.Equal(status, w.Code, "%s: %s", code, w.Body.String())
			var failure meta.ActionError
			require.NoError(json.Unmarshal(w.Body.Bytes(), &failure))
			require.NotNil(failure.Result.Receipt)
			assert.Equal(t, int64(3), failure.Result.Receipt.ID)
		}
	}
}
func TestCommentTriageRoute(t *testing.T) {
	assert := assert.New(t)
	st := &commentHTTPStore{}
	s := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	w := commentHTTPCall(s, "PUT", "/api/v1/messages/7/comment-triage", `{"state":"handled","expect":"open"}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.JSONEq(`{"state":"handled"}`, w.Body.String())
	for _, body := range []string{`{"state":"bad"}`, `{"state":"open","expect":"bad"}`, `{"state":"open","extra":true}`} {
		assert.Equal(400, commentHTTPCall(s, "PUT", "/api/v1/messages/7/comment-triage", body).Code)
	}
	assert.Equal(1, st.calls)
}
func TestCommentQueueRoutePagination(t *testing.T) {
	assert := assert.New(t)
	st := &commentHTTPStore{}
	s := NewServer(&config.Config{}, st, nil, testLogger())
	w := commentHTTPCall(s, "GET", "/api/v1/comment-queue?source=app:9:page:100&state=all&limit=1&before_id=3", "")
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(CommentQueueRequest{Source: "app:9:page:100", State: "all", Limit: 1, BeforeID: 3}, st.filter)
	assert.Contains(w.Body.String(), `"next_before_id":7`)
	for _, query := range []string{"limit=abc", "limit=-1", "limit=201", "before_id=-1", "state=bad"} {
		assert.Equal(400, commentHTTPCall(s, "GET", "/api/v1/comment-queue?"+query, "").Code)
	}
}
