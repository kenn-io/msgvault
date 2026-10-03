package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
)

type tagsHTTPStore struct {
	mockStore

	calls  int
	change *emailtags.Change
	fail   error
}

func (s *tagsHTTPStore) MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error) {
	s.calls++
	s.change = change
	result := &emailtags.Result{MessageID: id, SourceID: 2, Provider: "imap", Mailbox: mailbox, Tags: []string{"Next"}, Verified: true}
	if s.fail != nil {
		return result, s.fail
	}
	return result, nil
}
func TestEmailTagsRoutesAndValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &tagsHTTPStore{}
	srv := NewServer(&config.Config{}, st, newMockScheduler(), testLogger())
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	read := call(http.MethodGet, "/api/v1/messages/7/tags?mailbox=INBOX", "")
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	var result emailtags.Result
	require.NoError(json.Unmarshal(read.Body.Bytes(), &result))
	assert.Equal(int64(7), result.MessageID)
	assert.Equal("INBOX", result.Mailbox)
	write := call(http.MethodPost, "/api/v1/messages/7/tags", `{"add":["Next"],"dry_run":true}`)
	require.Equal(http.StatusOK, write.Code, write.Body.String())
	require.NotNil(st.change)
	assert.True(st.change.DryRun)
	calls := st.calls
	for _, body := range []string{`{}`, `{"add":["Next"],"remove":["Next"]}`, `{"add":["Next"],"unknown":true}`, `{"add":["Next"]} {}`} {
		got := call(http.MethodPost, "/api/v1/messages/7/tags", body)
		assert.Equal(http.StatusBadRequest, got.Code, got.Body.String())
	}
	assert.Equal(calls, st.calls)
	assert.Equal(http.StatusBadRequest, call(http.MethodGet, "/api/v1/messages/0/tags", "").Code)
}
func TestEmailTagsRoutesPreservePartialError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	partial := &emailtags.Result{Provider: "imap", Tags: []string{"Old", "Next"}}
	st := &tagsHTTPStore{fail: emailtags.Failure("remote_unknown", "read current tags before retrying", partial, nil)}
	srv := NewServer(&config.Config{}, st, newMockScheduler(), testLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/7/tags", strings.NewReader(`{"add":["Next"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusBadGateway, w.Code, w.Body.String())
	var failure emailtags.Error
	require.NoError(json.Unmarshal(w.Body.Bytes(), &failure))
	assert.Equal("remote_unknown", failure.Code)
	require.NotNil(failure.Result)
	assert.Equal(partial.Tags, failure.Result.Tags)
}
func TestEmailTagsDelegationIsDenied(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, reg := newTestServerWithAgentGrants(t)
	_, secret, _, err := reg.Issue("tags", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 2, Type: "imap", Identifier: "owner@example.test"}})
	require.NoError(err)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "/api/v1/messages/7/tags", strings.NewReader(`{"add":["Next"]}`))
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(http.StatusUnauthorized, w.Code, w.Body.String())
	}
}
