package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

func TestLoopKataSettingsDetectProjectChangedOutsideHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeProject := func(project string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte("[integrations.kata]\ndefault_project = \""+project+"\"\n"), 0o600))
	}
	writeProject("first-project")
	cfg, err := config.Load(path, "")
	require.NoError(t, err)
	server := api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler))
	readSettings := func() api.SettingsResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var settings api.SettingsResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &settings))
		return settings
	}
	assert.False(t, readSettings().PendingRestart)
	writeProject("second-project")
	assert.True(t, readSettings().PendingRestart, "a persisted project must not be disclosed as the running write target before restart")
	writeProject("first-project")
	assert.False(t, readSettings().PendingRestart, "restoring the running target removes this mismatch")
}
