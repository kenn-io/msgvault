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
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

type fakeAgendaIdentityStore struct{}

func (fakeAgendaIdentityStore) ListPersonUIDsContext(context.Context, int64) ([]string, error) {
	return []string{"person-a"}, nil
}

func agendaBackendConfig(server *httptest.Server) config.TaskIntegrationConfig {
	return config.TaskIntegrationConfig{
		Enabled: true, Endpoint: server.URL, APIKey: "secret", DefaultProject: " msgvault ",
	}
}

func TestPersonAgendaBackendRejectsGenericTaskService(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/capabilities":
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Capabilities{
				ProtocolVersion: taskclient.ProtocolVersion, RevisionReads: true, ConditionalMutation: true,
				ConflictResponses: true, IdempotentCreation: true, ProjectOperations: true, MetadataOperations: true,
			}))
		case "/api/v1/projects/msgvault":
			w.Header().Set("ETag", `"project-revision"`)
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Project{ID: "project-uid", Name: "msgvault"}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := personAgendaBackend{config: agendaBackendConfig(server), people: fakeAgendaIdentityStore{}}
	list := "follow up"

	operations := []struct {
		name string
		call func() error
	}{
		{"list", func() error { _, err := backend.List(t.Context(), 42); return err }},
		{"create", func() error {
			_, err := backend.Create(t.Context(), 42, "request-1", personagenda.CreateInput{Title: "Synthetic note"})
			return err
		}},
		{"link", func() error { _, err := backend.Link(t.Context(), 42, "task-1", ""); return err }},
		{"update", func() error {
			_, err := backend.Update(t.Context(), 42, "task-1", personagenda.UpdateInput{List: &list})
			return err
		}},
		{"unlink", func() error { _, err := backend.Unlink(t.Context(), 42, "task-1"); return err }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			err := operation.call()
			require.ErrorIs(t, err, taskclient.ErrIncompatible)

			response := httptest.NewRecorder()
			writePersonAgendaError(response, err)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		})
	}
}

func TestPersonAgendaBackendAcceptsNativeKataService(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/capabilities":
			http.NotFound(w, r)
		case "/api/v1/health":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "schema_version": 28, "version": "v0.18.0", "uptime": "1s",
				"started_at": "2026-09-22T00:00:00Z", "api_schema_version": "0.21.0",
			}))
		case "/api/v1/instance":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"instance_uid": "instance-1", "version": "0.18.0",
				"auth": map[string]any{}, "web_ui_capabilities": map[string]any{},
			}))
		case "/api/v1/projects":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"projects": []map[string]any{{
				"id": 1, "uid": "project-uid", "name": "msgvault", "active": true, "revision": 4,
				"created_at": "2026-09-22T00:00:00Z", "metadata": map[string]any{},
			}}}))
		case "/api/v1/projects/1/issues":
			assert.Equal("msgvault.person=person-a", r.URL.Query().Get("meta"))
			assert.Equal("open", r.URL.Query().Get("status"))
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"issues": []any{}}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := personAgendaBackend{config: agendaBackendConfig(server), people: fakeAgendaIdentityStore{}}

	service, err := backend.service(t.Context())
	require.NoError(err)
	assert.Equal("msgvault", service.Project)
	require.NotNil(service.Tasks)

	result, err := backend.List(t.Context(), 42)
	require.NoError(err)
	assert.Equal("msgvault", result.Project)
	assert.Empty(result.Items)
}
