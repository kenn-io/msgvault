package taskclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These responses exercise compatibility and error classification at the native
// HTTP boundary. The personagenda package runs issue operations against real Kata.
func TestConnectKataChecksNativeSchemaAndProject(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, schema, project string
		healthStatus          int
		wantErr               error
		wantState             State
	}{
		{"ready", "0.21.0", "msgvault", 200, nil, StateReady},
		{"newer schema", "0.22.0", "msgvault", 200, nil, StateReady},
		{"old schema", "0.20.0", "msgvault", 200, ErrIncompatible, StateIncompatible},
		{"malformed schema", "not-a-version", "msgvault", 200, ErrIncompatible, StateIncompatible},
		{"missing native API", "", "msgvault", 404, ErrIncompatible, StateIncompatible},
		{"authentication", "", "msgvault", 401, ErrAuthenticationRequired, StateAuthenticationRequired},
		{"unavailable", "", "msgvault", 503, ErrUnreachable, StateUnreachable},
		{"wrong project", "0.21.0", "other", 200, ErrWrongProject, StateWrongProject},
		{"missing project", "0.21.0", "", 200, ErrWrongProject, StateWrongProject},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					w.WriteHeader(tt.healthStatus)
					if tt.healthStatus == 200 {
						writeTestJSON(t, w, map[string]any{"api_schema_version": tt.schema})
					}
				case "/api/v1/projects":
					writeTestJSON(t, w, map[string]any{"projects": []any{map[string]any{"id": 1, "uid": "project-uid", "name": "msgvault", "active": true}}})
				default:
					assert.Fail("unexpected native request", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			cfg := IntegrationConfig{Enabled: true, Endpoint: server.URL, DefaultProject: tt.project, HTTPClient: server.Client()}
			client, err := ConnectKata(t.Context(), cfg)
			if tt.wantErr == nil {
				require.NoError(err)
				require.NotNil(client)
			} else {
				require.ErrorIs(err, tt.wantErr)
				assert.Nil(client)
			}
			assert.Equal(tt.wantState, EvaluateKata(t.Context(), cfg).State)
		})
	}
}

func TestKataRequiresExplicitEndpoint(t *testing.T) {
	t.Parallel()
	_, err := ConnectKata(t.Context(), IntegrationConfig{Enabled: true, DefaultProject: "msgvault", DescriptorPath: "unused.json"})
	require.ErrorIs(t, err, ErrInsecureEndpoint)
	assert.Equal(t, StateDisabled, EvaluateKata(t.Context(), IntegrationConfig{}).State)
}
