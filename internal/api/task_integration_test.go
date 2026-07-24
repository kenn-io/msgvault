package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/taskclient"
)

func TestTaskIntegrationStatusAndTestStayServerSide(t *testing.T) {
	var calls int
	var received taskclient.IntegrationConfig
	probe := TaskIntegrationProbe(func(_ context.Context, cfg taskclient.IntegrationConfig) taskclient.Status {
		calls++
		received = cfg
		return taskclient.Status{
			State:   taskclient.StateReady,
			Project: "test-project",
			Message: "Task integration is ready.",
		}
	})
	cfg := &config.Config{
		Server: config.ServerConfig{APIKey: "msgvault-test-api-key"},
		Integrations: config.IntegrationsConfig{Tasks: config.TaskIntegrationConfig{
			Enabled:        true,
			Endpoint:       "https://tasks.example.com",
			APIKey:         "upstream-secret-test-key",
			DefaultProject: "test-project",
		}},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:               cfg,
		Logger:               testLogger(),
		TaskIntegrationProbe: probe,
	})

	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "status", method: http.MethodGet, path: taskIntegrationStatusPath},
		{name: "test", method: http.MethodPost, path: taskIntegrationTestPath},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			req := httptest.NewRequest(test.method, test.path, nil)
			req.Header.Set("X-Api-Key", "msgvault-test-api-key")
			resp := httptest.NewRecorder()

			srv.Router().ServeHTTP(resp, req)

			requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
			assertions.Equal("no-store", resp.Header().Get("Cache-Control"))
			var body TaskIntegrationStatusResponse
			requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
			assertions.Equal(taskclient.StateReady, body.State)
			assertions.Equal("test-project", body.Project)
			assertions.NotContains(resp.Body.String(), "upstream-secret-test-key")
			assertions.NotContains(resp.Body.String(), "tasks.example.com")
		})
	}

	assert.Equal(t, 2, calls)
	assert.Equal(t, "upstream-secret-test-key", received.APIKey)
	assert.Equal(t, "https://tasks.example.com", received.Endpoint)
}

func TestTaskIntegrationStatusMapsRuntimeConfigWithoutAmbientDiscovery(t *testing.T) {
	var received taskclient.IntegrationConfig
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{
			Server: config.ServerConfig{},
			Integrations: config.IntegrationsConfig{Tasks: config.TaskIntegrationConfig{
				Enabled:        false,
				DefaultProject: "test-project",
			}},
		},
		Logger: testLogger(),
		TaskIntegrationProbe: func(_ context.Context, cfg taskclient.IntegrationConfig) taskclient.Status {
			received = cfg
			return taskclient.Status{State: taskclient.StateDisabled, Message: "Task integration is disabled."}
		},
	})
	req := httptest.NewRequest(http.MethodGet, taskIntegrationStatusPath, nil)
	req.RemoteAddr = "127.0.0.1:32145"
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.False(t, received.Enabled)
	assert.Empty(t, received.DescriptorPath)
}
