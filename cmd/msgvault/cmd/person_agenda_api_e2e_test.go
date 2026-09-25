package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

// nativeKataStub serves the endpoints taskclient.ConnectKata probes for a native Kata task service, and answers person-filtered agenda
// listing with an empty issue page.
func nativeKataStub(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/capabilities":
			http.NotFound(w, r)
		case "/api/v1/health":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "schema_version": 28, "version": "v0.18.0", "uptime": "1s",
				"started_at": "2026-09-22T00:00:00Z", "api_schema_version": "0.21.0",
			}))
		case "/api/v1/instance":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"instance_uid": "instance-1", "version": "0.18.0",
				"auth": map[string]any{}, "web_ui_capabilities": map[string]any{},
			}))
		case "/api/v1/projects":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"projects": []map[string]any{{
				"id": 1, "uid": "project-uid", "name": "msgvault", "active": true, "revision": 4,
				"created_at": "2026-09-22T00:00:00Z", "metadata": map[string]any{},
			}}}))
		case "/api/v1/projects/1/issues":
			assert.True(t, strings.HasPrefix(r.URL.Query().Get("meta"), "msgvault.person="))
			assert.Equal(t, "open", r.URL.Query().Get("status"))
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"issues": []any{}}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestPersonAgendaEndpointServesThroughTheProductionAdapter pins that the live
// agenda route is reachable in the daemon. It builds the server exactly as
// serve.go does -- Store: &storeAPIAdapter{...} -- and asserts the agenda list
// answers over HTTP. When the adapter lacked ListPersonUIDsContext, the backend
// initialized as nil and every agenda endpoint answered 503
// task_integration_unavailable no matter how the task service was configured.
func TestPersonAgendaEndpointServesThroughTheProductionAdapter(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "agenda-adapter@example.test", "Agenda Adapter")
	require.NoError(err, "create participant")
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err, "create person")

	kata := nativeKataStub(t)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Integrations: config.IntegrationsConfig{Kata: config.TaskIntegrationConfig{
			Enabled: true, Endpoint: kata.URL, APIKey: "secret", DefaultProject: "msgvault",
		}}},
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	response, err := http.Get(
		fmt.Sprintf("%s/api/v1/people/%d/agenda", httpSrv.URL, person.ID))
	require.NoError(err, "GET /people/{id}/agenda")
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(err, "read agenda response")
	require.Equalf(http.StatusOK, response.StatusCode,
		"the daemon's own adapter must back the agenda route, not report it unavailable: %s %s",
		response.Status, body)
	var result api.PersonAgendaResult
	require.NoError(json.Unmarshal(body, &result), "decode agenda result")
	assert.Equal(t, "msgvault", result.Project)
	assert.Empty(t, result.Items)
}
