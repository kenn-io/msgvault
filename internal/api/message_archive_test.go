package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
)

type archiveHTTPStore struct {
	mockStore

	calls int
	fail  error
}

func (s *archiveHTTPStore) ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange, _ *agentgrant.Grant, acquire func(context.Context) (func(), error)) (*emailtags.ArchiveResult, error) {
	if !change.DryRun {
		release, err := acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	s.calls++
	return &emailtags.ArchiveResult{MessageID: id, Archived: *change.Archived, DryRun: change.DryRun}, s.fail
}
func TestArchiveMessageRouteValidationAndGate(t *testing.T) {
	assertionsForTest := assert.New(t) //nolint:paralleltest // swaps the gate wait bound
	old := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = old })
	st := &archiveHTTPStore{}
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger(), OperationGate: gate})
	call := func(id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/messages/"+id+"/archive", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	for _, body := range []string{`{}`, `{"archived":null}`} {
		response := call("7", body)
		assertionsForTest.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
	assertionsForTest.Equal(http.StatusBadRequest, call("0", `{"archived":true}`).Code)
	assertionsForTest.Zero(st.calls)
	st.fail = emailtags.Failure("archive_unsupported", "synthetic failure", nil, nil)
	response := call("7", `{"archived":false}`)
	assertionsForTest.Equal(http.StatusNotImplemented, response.Code, response.Body.String())
	st.fail = nil
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "sync")
	require.True(t, ok)
	defer release()
	preview := call("7", `{"archived":true,"dry_run":true}`)
	assertionsForTest.Equal(http.StatusOK, preview.Code, preview.Body.String())
	write := call("7", `{"archived":true}`)
	assertionsForTest.Equal(http.StatusServiceUnavailable, write.Code, write.Body.String())
}
