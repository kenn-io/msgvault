package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
)

func TestTelegramCredentialDaemonHandoff(t *testing.T) {
	const secret = "synthetic-session-secret"
	calls := 0
	st := &mockStore{runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
		calls++
		assert.Equal(t, secret, req.Env[clirun.EnvTelegramSession])
		return emit(CLIRunEvent{Type: "stdout", Data: "registered"})
	}}
	var logs bytes.Buffer
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	for _, tc := range []struct {
		command string
		status  int
	}{{"add-telegram", http.StatusOK}, {"add-discord", http.StatusBadRequest}} {
		body, err := json.Marshal(CLIRunRequest{Args: []string{tc.command}, Env: map[string]string{clirun.EnvTelegramSession: secret}})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		{
			assert := assert.New(t)
			assert.Equal(tc.status, resp.Code)
		}
		assert.NotContains(t, resp.Body.String(), secret)
	}
	assert.Equal(t, 1, calls)
	assert.NotContains(t, logs.String(), secret)
}
