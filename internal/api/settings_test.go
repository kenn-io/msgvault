package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestGetSettingsUsesAllowlistETagAndSecretStates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newSettingsTestServer(t, "# keep\n[web]\ntheme = \"dark\"\n"+
		"[server]\napi_key = \"test-api-key\"\n"+
		"[integrations.tasks]\napi_key = \"task-secret\"\n"+
		"[unsupported]\nprivate_value = \"must-not-leak\"\n")
	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "test-api-key")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	assert.NotEmpty(resp.Header().Get("ETag"))
	assert.Equal("no-store", resp.Header().Get("Cache-Control"))

	var body SettingsResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	require.NotNil(byKey["web.theme"].Value)
	require.NotNil(byKey["web.theme"].Value.String)
	assert.Equal("dark", *byKey["web.theme"].Value.String)
	assert.Equal(&SecretSettingState{Configured: true}, byKey["server.api_key"].Secret)
	assert.Nil(byKey["server.api_key"].Value)
	assert.Equal(&SecretSettingState{Configured: true}, byKey["integrations.tasks.api_key"].Secret)
	require.NotNil(byKey["server.trusted_proxies"].Value)
	assert.NotNil(byKey["server.trusted_proxies"].Value.Strings)
	assert.NotContains(byKey, "unsupported.private_value")
	for _, setting := range body.Settings {
		assert.True(setting.RestartRequired, setting.Key)
	}
	assert.NotContains(resp.Body.String(), "test-api-key")
	assert.NotContains(resp.Body.String(), "task-secret")
	assert.NotContains(resp.Body.String(), "must-not-leak")
}

func TestPatchSettingsRequiresMatchingETag(t *testing.T) {
	assert := assert.New(t)
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")

	missing := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "", "")
	assert.Equal(http.StatusPreconditionRequired, missing.Code, missing.Body.String())

	mismatch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "\"sha256-stale\"", "")
	assert.Equal(http.StatusPreconditionFailed, mismatch.Code, mismatch.Body.String())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal("[web]\ntheme = \"system\"\n", string(got))
}

func TestPatchSettingsPreservesFileAndReturnsNewETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t, "# operator comment\n[unknown]\nkeep = true\n\n"+
		"[web]\ntheme = \"system\" # display\n")
	if runtime.GOOS != "windows" {
		require.NoError(os.Chmod(path, 0o640))
	}
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	etag := get.Header().Get("ETag")

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), etag, "")
	require.Equal(http.StatusOK, patch.Code, patch.Body.String())
	assert.NotEqual(etag, patch.Header().Get("ETag"))
	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal("# operator comment\n[unknown]\nkeep = true\n\n[web]\ntheme = \"dark\" # display\n", string(got))
	if runtime.GOOS != "windows" {
		// Unix mode preservation. Windows security lives in the DACL, which
		// the config package's own Windows tests verify; Stat mode bits there
		// are synthetic.
		info, err := os.Stat(path)
		require.NoError(err)
		assert.Equal(os.FileMode(0o640), info.Mode().Perm())
	}
}

func TestPatchSettingsValidatesWholeCandidateAndRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{
			name:   "invalid catalog value",
			body:   `{"updates":[{"key":"analytics.engine","value":{"string":"invalid"}}]}`,
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "unsupported key",
			body:   `{"updates":[{"key":"unsupported.private_value","value":{"string":"changed"}}]}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "secret sent as ordinary value",
			body:   `{"updates":[{"key":"server.api_key","value":{"string":"leak"}}]}`,
			status: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := "[analytics]\nengine = \"auto\"\n[unsupported]\nprivate_value = \"keep\"\n"
			srv, path := newSettingsTestServer(t, before)
			get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
			resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath, []byte(tt.body),
				get.Header().Get("ETag"), "")
			assert.Equal(t, tt.status, resp.Code, resp.Body.String())
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, string(got))
		})
	}
}

func TestPatchSettingsProtectsAPIKeyRestartSequencing(t *testing.T) {
	t.Run("confirmation required", func(t *testing.T) {
		srv, _ := newSettingsTestServer(t, "[server]\napi_key = \"old-key\"\n")
		get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "old-key")
		resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
			[]byte(`{"updates":[{"key":"server.api_key","secret":{"action":"set","value":"new-key"}}]}`),
			get.Header().Get("ETag"), "old-key")
		assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
	})

	t.Run("full candidate prevents remote self lockout", func(t *testing.T) {
		srv, _ := newSettingsTestServer(t, "[server]\nbind_addr = \"0.0.0.0\"\napi_key = \"old-key\"\n")
		get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "old-key")
		resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
			[]byte(`{"confirm_api_key_restart":true,"updates":[{"key":"server.api_key","secret":{"action":"clear"}}]}`),
			get.Header().Get("ETag"), "old-key")
		assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	})

	t.Run("new key remains pending until restart", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		srv, path := newSettingsTestServer(t, "[server]\napi_key = \"old-key\"\n")
		get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "old-key")
		resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
			[]byte(`{"confirm_api_key_restart":true,"updates":[{"key":"server.api_key","secret":{"action":"set","value":"new-key"}}]}`),
			get.Header().Get("ETag"), "old-key")
		require.Equal(http.StatusOK, resp.Code, resp.Body.String())
		assert.Equal("old-key", srv.cfg.Server.APIKey)
		got, err := os.ReadFile(path)
		require.NoError(err)
		assert.Contains(string(got), "api_key = \"new-key\"")
		assert.NotContains(resp.Body.String(), "new-key")

		stillActive := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "old-key")
		assert.Equal(http.StatusOK, stillActive.Code, stillActive.Body.String())
		var persisted SettingsResponse
		require.NoError(json.Unmarshal(stillActive.Body.Bytes(), &persisted))
		assert.True(persisted.PendingRestart)

		restartedConfig, err := config.Load(path, "")
		require.NoError(err)
		restarted := NewServer(restartedConfig, nil, nil, slog.New(slog.DiscardHandler))
		oldSession := performSessionRequest(t, restarted, http.MethodGet, sessionPath, nil,
			http.Header{"Cookie": []string{requireSessionCookie(t, performSessionRequest(
				t, srv, http.MethodPost, sessionLoginPath, []byte(`{"api_key":"old-key"}`), nil, false,
			)).String()}}, false)
		require.Equal(http.StatusOK, oldSession.Code, oldSession.Body.String())
		assert.Equal(AuthModeRequired, decodeSessionStatus(t, oldSession).AuthMode)
		assert.Empty(oldSession.Header().Values("Set-Cookie"), "bootstrap must not reissue authority for the stale cookie")

		oldKey := performSettingsRequest(t, restarted, http.MethodGet, settingsPath, nil, "", "old-key")
		assert.Equal(http.StatusUnauthorized, oldKey.Code, oldKey.Body.String())
		newKey := performSettingsRequest(t, restarted, http.MethodGet, settingsPath, nil, "", "new-key")
		assert.Equal(http.StatusOK, newKey.Code, newKey.Body.String())
	})
}

func TestPatchSettingsClearsTaskAPIKeyWhenEndpointOriginChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}}]}`)
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assert.Contains(string(got), "api_key = \"\"")
	assert.NotContains(string(got), "task-secret")

	var body SettingsResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	assert.Equal(&SecretSettingState{Configured: false}, byKey["integrations.tasks.api_key"].Secret)
	assert.True(body.PendingRestart)
}

func TestPatchSettingsKeepsNewTaskAPIKeyProvidedWithEndpointChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[`+
			`{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}},`+
			`{"key":"integrations.tasks.api_key","secret":{"action":"set","value":"rotated-secret"}}]}`)
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assert.Contains(string(got), "api_key = \"rotated-secret\"")

	var body SettingsResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assert.Equal(&SecretSettingState{Configured: true},
		settingsByKey(body.Settings)["integrations.tasks.api_key"].Secret)
}

func TestPatchSettingsRetainsTaskAPIKeyWhenEndpointOriginIsUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "identical endpoint", endpoint: "https://tasks.example.com/api"},
		{name: "same origin different path", endpoint: "https://tasks.example.com/v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, path := newSettingsTestServer(t,
				"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

			resp := patchSettings(t, srv, fmt.Sprintf(
				`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":%q}}]}`, tt.endpoint))
			require.Equal(http.StatusOK, resp.Code, resp.Body.String())

			got, err := os.ReadFile(path)
			require.NoError(err)
			assert.Contains(string(got), "api_key = \"task-secret\"")

			var body SettingsResponse
			require.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
			assert.Equal(&SecretSettingState{Configured: true},
				settingsByKey(body.Settings)["integrations.tasks.api_key"].Secret)
		})
	}
}

func TestPatchSettingsEndpointChangeWithoutStoredCredentialAddsNoKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}}]}`)
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assert.NotContains(string(got), "api_key")
}

func TestPatchSettingsClearsEmbeddingsAPIKeyEnvWhenEndpointOriginChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[vector.embeddings]\nendpoint = \"https://embed.example.com/v1\"\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://elsewhere.example.net/v1"}}]}`)
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(got), "endpoint = \"https://elsewhere.example.net/v1\"")
	assert.Contains(string(got), "api_key_env = \"\"")

	var body SettingsResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	require.NotNil(byKey["vector.embeddings.api_key_env"].Value)
	require.NotNil(byKey["vector.embeddings.api_key_env"].Value.String)
	assert.Empty(*byKey["vector.embeddings.api_key_env"].Value.String)
}

func TestPatchSettingsRejectsEmbeddingsAPIKeyEnvUpdates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	before := "[vector.embeddings]\nendpoint = \"https://embed.example.com/v1\"\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n"
	srv, path := newSettingsTestServer(t, before)

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.api_key_env","value":{"string":"AWS_SECRET_ACCESS_KEY"}}]}`)
	require.Equal(http.StatusBadRequest, resp.Code, resp.Body.String())
	assert.Contains(resp.Body.String(), "edit config.toml")

	got, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(before, string(got))
}

func TestSettingsErrorsAreNotCached(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "", "")

	assert.Equal(t, http.StatusPreconditionRequired, resp.Code, resp.Body.String())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestSettingsMiddlewareErrorsAreNotCached(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[server]\napi_key = \"test-api-key\"\n")
	login := performSessionRequest(t, srv, http.MethodPost, sessionLoginPath,
		[]byte(`{"api_key":"test-api-key"}`), nil, false)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	cookie := requireSessionCookie(t, login)
	resp := performSessionRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		http.Header{"Cookie": []string{cookie.String()}}, false)

	assert.Equal(t, http.StatusForbidden, resp.Code, resp.Body.String())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestPatchSettingsRejectsTrailingJSON(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]} {}`),
		get.Header().Get("ETag"), "")

	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
}

func TestPatchSettingsClassifiesFilesystemFailureAsServerError(t *testing.T) {
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	blockSettingsConfigFilesystem(t, path)

	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		get.Header().Get("ETag"), "")

	assert.Equal(t, http.StatusInternalServerError, resp.Code, resp.Body.String())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestPatchSettingsMarksRestartPendingWhenPublishedWriteReturnsError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	srv.settingsConfigEditor = func(configPath, ifMatch string, edits []config.Edit) (config.ConfigFile, error) {
		require.Equal(path, configPath)
		require.Equal(get.Header().Get("ETag"), ifMatch)
		require.Len(edits, 1)
		require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"dark\"\n"), 0o600))
		return config.ConfigFile{}, fmt.Errorf("%w: cleanup failed", config.ErrConfigChanged)
	}

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		get.Header().Get("ETag"), "")
	assert.Equal(http.StatusInternalServerError, patch.Code, patch.Body.String())

	after := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(http.StatusOK, after.Code, after.Body.String())
	var persisted SettingsResponse
	require.NoError(json.Unmarshal(after.Body.Bytes(), &persisted))
	assert.True(persisted.PendingRestart)
}

func TestPatchSettingsMarksRestartPendingBeforeLoadingCommittedSnapshot(t *testing.T) {
	assert := assert.New(t)
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	srv.settingsConfigEditor = func(string, string, []config.Edit) (config.ConfigFile, error) {
		return config.ConfigFile{
			LogicalPath: "config.toml",
			Path:        "config.toml",
			Content:     []byte("invalid = ["),
			ETag:        `"sha256-committed"`,
			Exists:      true,
		}, nil
	}

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		get.Header().Get("ETag"), "")
	assert.Equal(http.StatusInternalServerError, patch.Code, patch.Body.String())
	assert.True(srv.settingsPendingRestart.Load())
}

func TestPatchSettingsPrefersChangedOutcomeOverConflictClassification(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	srv.settingsConfigEditor = func(string, string, []config.Edit) (config.ConfigFile, error) {
		return config.ConfigFile{}, errors.Join(config.ErrConfigChanged, config.ErrConfigConflict)
	}

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		get.Header().Get("ETag"), "")
	assert.Equal(t, http.StatusInternalServerError, patch.Code, patch.Body.String())
}

func TestSettingsOpenAPIContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	doc := OpenAPIDocument()
	require.NotNil(doc.Paths[settingsPath])
	get := doc.Paths[settingsPath].Get
	patch := doc.Paths[settingsPath].Patch
	require.NotNil(get)
	require.NotNil(patch)
	assert.Contains(get.Responses["200"].Headers, "ETag")
	require.Len(patch.Parameters, 1)
	assert.Equal("If-Match", patch.Parameters[0].Name)
	assert.Equal("header", patch.Parameters[0].In)
	assert.True(patch.Parameters[0].Required)
	for _, status := range []string{"400", "409", "412", "422", "428"} {
		assert.Contains(patch.Responses, status)
	}
	assert.Equal("1.25.0", doc.Info.Version)

	settingValue := doc.Components.Schemas.Map()["SettingValue"]
	require.NotNil(settingValue)
	assert.Len(settingValue.OneOf, 5)
	assert.Empty(settingValue.Properties)
	for _, arm := range settingValue.OneOf {
		assert.Len(arm.Required, 1)
		assert.Equal([]string{arm.Required[0]}, arm.Required)
		assert.Equal(false, arm.AdditionalProperties)
	}
	setting := doc.Components.Schemas.Map()["Setting"]
	require.NotNil(setting)
	assert.ElementsMatch([]any{"browser", "server", "archive", "search", "sources", "integrations"}, setting.Properties["group"].Enum)
	assert.ElementsMatch([]any{"string", "integer", "number", "boolean", "string_array", "secret"}, setting.Properties["kind"].Enum)
	patchRequest := doc.Components.Schemas.Map()["SettingsPatchRequest"]
	require.NotNil(patchRequest)
	assert.False(patchRequest.Properties["updates"].Nullable)
}

func newSettingsTestServer(t *testing.T, content string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	cfg, err := config.Load(path, "")
	require.NoError(t, err)
	logger := slog.New(slog.DiscardHandler)
	return NewServer(cfg, nil, nil, logger), path
}

//nolint:unparam // Keep the actual route visible at each HTTP test call site.
func performSettingsRequest(
	t *testing.T,
	srv *Server,
	method string,
	path string,
	body []byte,
	ifMatch string,
	apiKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

// patchSettings performs a GET to obtain the current ETag and issues a PATCH
// with the supplied JSON body against an unauthenticated test server.
func patchSettings(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	return performSettingsRequest(t, srv, http.MethodPatch, settingsPath, []byte(body),
		get.Header().Get("ETag"), "")
}

func settingsByKey(settings []Setting) map[string]Setting {
	result := make(map[string]Setting, len(settings))
	for _, setting := range settings {
		result[setting.Key] = setting
	}
	return result
}
