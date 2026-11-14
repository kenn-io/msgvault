package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/taskclient"
)

func TestPrivateNetworkKataAPIPaths(t *testing.T) {
	requirements := require.New(t)
	cfg := &config.Config{}
	cfg.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: "http://kata:7777", TrustPrivateNetwork: true}
	// No key means authentication_required before dialing. An omitted flag
	// instead fails endpoint validation, so this covers both production paths.
	_, err := connectKata(t.Context(), cfg.Integrations.Kata)
	requirements.ErrorIs(err, taskclient.ErrAuthenticationRequired)
	s := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{}, Logger: testLogger()})
	response := httptest.NewRecorder()
	s.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/integrations/kata/status", nil))
	requirements.Equal(http.StatusOK, response.Code)
	var status TaskIntegrationStatusResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	assert.Equal(t, taskclient.StateAuthenticationRequired, status.State)
}

func TestPrivateNetworkSettingsZoneChangeClearsKey(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, replacement, wantKey string
	}{
		{"different zone", "http://[fe80::1%25NET0]:7777", "", ""},
		{"replacement", "http://[fe80::1%25NET0]:7777", `,{"key":"integrations.kata.api_key","secret":{"action":"set","value":"replacement-key"}}`, "replacement-key"},
		{"same zone path", "http://[fe80::1%25net0]:7777/path", "", "saved-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, path := newSettingsTestServer(t, "[integrations.kata]\nendpoint = 'http://[fe80::1%25net0]:7777'\ntrust_private_network = true\napi_key = 'saved-key'\n")
			resp := patchSettings(t, srv, fmt.Sprintf(`{"updates":[{"key":"integrations.kata.endpoint","value":{"string":%q}}%s]}`, tc.endpoint, tc.replacement))
			require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
			cfg, err := config.Load(path, "")
			require.NoError(t, err)
			assert.Equal(t, tc.wantKey, cfg.Integrations.Kata.APIKey)
		})
	}
}
