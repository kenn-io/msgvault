package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
)

type tagsHTTPStore struct {
	mockStore

	calls  int
	change *emailtags.MessageTagChange
	fail   error
}

func (s *tagsHTTPStore) MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string, acquireWrite func(context.Context) (func(), error)) (*emailtags.MessageTagResult, error) {
	if change != nil && !change.DryRun {
		release, err := acquireWrite(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	s.calls++
	s.change = change
	result := &emailtags.MessageTagResult{MessageID: id, SourceID: 2, Provider: "imap", Mailbox: mailbox, Tags: []string{"Next"}, Verified: true}
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
	var result emailtags.MessageTagResult
	require.NoError(json.Unmarshal(read.Body.Bytes(), &result))
	assert.Equal(int64(7), result.MessageID)
	assert.Equal("INBOX", result.Mailbox)
	write := call(http.MethodPost, "/api/v1/messages/7/tags", `{"add":["Next"],"dry_run":true}`)
	require.Equal(http.StatusOK, write.Code, write.Body.String())
	require.NotNil(st.change)
	assert.True(st.change.DryRun)
	calls := st.calls
	for _, body := range []string{`{"add":["Next"],"unknown":true}`, `{"add":["Next"]} {}`} {
		invalid := call(http.MethodPost, "/api/v1/messages/7/tags", body)
		assert.Equal(http.StatusBadRequest, invalid.Code, invalid.Body.String())
	}
	assert.Equal(calls, st.calls)
	assert.Equal(http.StatusBadRequest, call(http.MethodGet, "/api/v1/messages/0/tags", "").Code)
	partial := &emailtags.MessageTagResult{Provider: "imap", Tags: []string{"Old", "Next"}}
	for _, tc := range []struct {
		code   string
		status int
		result *emailtags.MessageTagResult
	}{
		{"invalid_tag", http.StatusBadRequest, nil},
		{"remote_unknown", http.StatusBadGateway, partial},
	} {
		st.fail = emailtags.Failure(tc.code, "read current tags before retrying", tc.result, nil)
		response := call(http.MethodPost, "/api/v1/messages/7/tags", `{"add":["Next"]}`)
		require.Equal(tc.status, response.Code, response.Body.String())
		var failure emailtags.MessageTagError
		require.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
		assert.Equal(tc.code, failure.Code)
		if tc.result != nil {
			require.NotNil(failure.Result)
			assert.Equal(tc.result.Tags, failure.Result.Tags)
		}
	}
}
func TestEmailTagsPreviewRunsWhileGateHeld(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	assert := assert.New(t)
	require := require.New(t)
	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "sync")
	require.True(ok)
	defer release()
	st := &tagsHTTPStore{}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), OperationGate: gate})
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/7/tags", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	preview := call(`{"add":["Next"],"dry_run":true}`)
	require.Equal(http.StatusOK, preview.Code, preview.Body.String())
	write := call(`{"add":["Next"]}`)
	assert.Equal(http.StatusServiceUnavailable, write.Code, write.Body.String())
	assert.Equal(1, st.calls, "the write must not reach the provider while the gate is held")
}
