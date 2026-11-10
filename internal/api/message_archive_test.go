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
func TestArchiveMessageRouteValidationAndGate(t *testing.T) { //nolint:paralleltest // swaps the gate wait bound
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
	for _, body := range []string{`{}`, `{"archived":null}`, `{"archived":"yes"}`, `{"archived":true,"unknown":true}`, `{"archived":true} {}`} {
		response := call("7", body)
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	}
	assert.Equal(t, http.StatusBadRequest, call("0", `{"archived":true}`).Code)
	assert.Zero(t, st.calls)
	for _, code := range []string{"archive_unsupported", "insufficient_scope", "message_not_found"} {
		st.fail = emailtags.Failure(code, "synthetic failure", nil, nil)
		response := call("7", `{"archived":false}`)
		expected := map[string]int{"archive_unsupported": 501, "insufficient_scope": 403, "message_not_found": 404}[code]
		assert.Equal(t, expected, response.Code, response.Body.String())
	}
	st.fail = nil
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "sync")
	require.True(t, ok)
	defer release()
	preview := call("7", `{"archived":false,"dry_run":true}`)
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	assert.Contains(t, preview.Body.String(), `"archived":false`)
	write := call("7", `{"archived":true}`)
	assert.Equal(t, http.StatusServiceUnavailable, write.Code, write.Body.String())
}
