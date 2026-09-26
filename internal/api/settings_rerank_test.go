package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/providercredentials"
)

func rawSettingsByKey(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var document struct {
		Settings []map[string]any `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(body, &document))
	byKey := make(map[string]map[string]any, len(document.Settings))
	for _, setting := range document.Settings {
		key, _ := setting["key"].(string)
		byKey[key] = setting
	}
	return byKey
}

func TestSettingsExposeRerankDefaults(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	srv, _ := newSettingsTestServer(t, "")

	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	byKey := rawSettingsByKey(t, get.Body.Bytes())
	for _, key := range []string{
		"vector.rerank.enabled", "vector.rerank.api_format", "vector.rerank.endpoint",
		"vector.rerank.api_key_env", "vector.rerank.api_key", "vector.rerank.model",
		"vector.rerank.candidates", "vector.rerank.max_candidate_chars",
		"vector.rerank.timeout", "vector.rerank.default",
	} {
		require.Contains(byKey, key)
		assert.NotEmpty(byKey[key]["label"], key)
		assert.NotEmpty(byKey[key]["description"], key)
	}
	assert.Equal(map[string]any{"boolean": false}, byKey["vector.rerank.enabled"]["value"])
	assert.Equal(map[string]any{"string": "cohere/rerank-4-pro"}, byKey["vector.rerank.model"]["value"])
	assert.Equal(map[string]any{"string": "https://openrouter.ai/api/v1"}, byKey["vector.rerank.endpoint"]["value"])
	assert.Equal(map[string]any{"configured": false, "source": "none"}, byKey["vector.rerank.api_key"]["secret"])
}

func TestSettingsRerankCredentialIsBoundToEndpointOrigin(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, "[vector.rerank]\nendpoint = \"https://first.example.test/api/v1\"\n")

	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/vector.rerank",
		[]byte(`{"value":"sk-rerank-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirements.Equal(http.StatusOK, set.Code, set.Body.String())
	stored, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.True(stored.Stored(providercredentials.VectorRerankID))

	moved := patchSettings(t, srv,
		`{"updates":[{"key":"vector.rerank.endpoint","value":{"string":"https://second.example.test/api/v1"}}]}`)
	requirements.Equal(http.StatusOK, moved.Code, moved.Body.String())
	assertions.NotContains(moved.Body.String(), "sk-rerank-secret")
	severed, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.False(severed.Stored(providercredentials.VectorRerankID),
		"a stored reranker key must never follow the endpoint to a new origin")
}

func TestSettingsRejectInvalidRerankValues(t *testing.T) {
	t.Parallel()
	for _, update := range []string{
		`{"key":"vector.rerank.candidates","value":{"integer":101}}`,
		`{"key":"vector.rerank.timeout","value":{"string":"soon"}}`,
		`{"key":"vector.rerank.endpoint","value":{"string":"https://user:pw@example.test/v1"}}`,
	} {
		t.Run(update, func(t *testing.T) {
			t.Parallel()
			srv, _ := newSettingsTestServer(t, "")
			response := patchSettings(t, srv, `{"updates":[`+update+`]}`)
			assert.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
		})
	}
}
