package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiagnoseFTSRunnerAdmissionAndReadOnlyGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	called := false
	st := &mockStore{runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
		called = true
		assert.Equal([]string{"diagnose-fts", "--limit=2", "--json"}, req.Args)
		return emit(CLIRunEvent{Type: "stdout", Data: `{"outcome":"incomplete"}`})
	}}
	srv := newCLIHandlerTestServer(st)
	body := `{"args":["diagnose-fts","--limit=2","--json"]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/run", strings.NewReader(body))
	_, skip, err := cliRunGateDecision(req, requestAuthentication{})
	require.NoError(err)
	assert.True(skip, "diagnostic reads do not acquire a mutation gate")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(http.StatusOK, resp.Code)
	assert.True(called)
	assert.Contains(resp.Body.String(), "incomplete")
}
