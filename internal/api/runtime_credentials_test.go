package api

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
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
)

func TestSettingsUsesMintedServerKeyWithoutSeedingConfig(t *testing.T) { //nolint:paralleltest // isolates the fallback home
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_HOME", filepath.Join(t.TempDir(), "unintended-default"))
	home := t.TempDir()
	cfg, err := config.Load("", home)
	require.NoError(err)
	cfg.Server.BindAddr = "0.0.0.0"
	require.NoError(cfg.PrepareServerKey())
	key := cfg.Server.AuthenticationKey()
	srv := NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler))
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", key)
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	var body SettingsResponse
	require.NoError(json.Unmarshal(get.Body.Bytes(), &body))
	assert.True(settingsByKey(body.Settings)["server.api_key"].Secret.Configured)
	_, err = os.Stat(cfg.ConfigFilePath())
	require.ErrorIs(err, os.ErrNotExist)
	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), get.Header().Get("ETag"), key)
	require.Equal(http.StatusOK, patch.Code, patch.Body.String())
	saved, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err)
	assert.NotContains(string(saved), key)
	assert.NotContains(patch.Body.String(), key)
}

func TestSettingsReloadAppliesAllowInsecureBeforeResolvingDefaultServerKey(t *testing.T) { //nolint:paralleltest // process environment and isolated home
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte("[server]\nbind_addr = \"0.0.0.0\"\n"), 0o600))
	t.Setenv("MSGVAULT_ALLOW_INSECURE", "false")
	initial, err := config.Load("", home)
	require.NoError(err)
	require.NoError(initial.PrepareServerKey())
	assert.NotEmpty(initial.Server.AuthenticationKey())

	t.Setenv("MSGVAULT_ALLOW_INSECURE", "true")
	effective, err := config.Load("", home)
	require.NoError(err)
	require.NoError(effective.ResolveServerKey())
	assert.Empty(effective.Server.AuthenticationKey(), "allow_insecure must suppress the persisted default key")
	srv := NewServer(effective, nil, nil, slog.New(slog.DiscardHandler))
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	var body SettingsResponse
	require.NoError(json.Unmarshal(get.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	assert.False(byKey["server.api_key"].Secret.Configured)
	require.NotNil(byKey["server.allow_insecure"].Value)
	require.NotNil(byKey["server.allow_insecure"].Value.Boolean)
	assert.True(*byKey["server.allow_insecure"].Value.Boolean)
}

func TestSettingsEditsAfterServerCredentialFileDisappears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	home := t.TempDir()
	keyPath := filepath.Join(home, "key")
	key := "synthetic-runtime-api-key"
	require.NoError(fileutil.SecureWriteFile(keyPath, []byte(key), 0o600))
	configPath := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte("[server]\napi_key_file = 'key'\n"), 0o600))
	cfg, err := config.Load(configPath, home)
	require.NoError(err)
	require.NoError(cfg.PrepareServerKey())
	srv := NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler))
	require.NoError(os.Remove(keyPath))
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", key)
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), get.Header().Get("ETag"), key)
	require.Equal(http.StatusOK, patch.Code, patch.Body.String())
	var body SettingsResponse
	require.NoError(json.Unmarshal(patch.Body.Bytes(), &body))
	assert.Equal("dark", *settingsByKey(body.Settings)["web.theme"].Value.String)
	assert.True(settingsByKey(body.Settings)["server.api_key"].Secret.Configured)
	assert.NotContains(patch.Body.String(), key)
}

func TestSettingsPreservesServeFlagsOverInvalidEnvironment(t *testing.T) { //nolint:paralleltest // process environment
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_BIND_ADDR", " ")
	t.Setenv("MSGVAULT_API_PORT", "invalid-port")
	bind, port := "127.0.0.2", 8181
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte("[server]\nbind_addr = '127.0.0.1'\napi_port = 8080\n"), 0o600))
	cfg, err := config.LoadWithOverrides(path, home, config.RuntimeOverrides{BindAddr: &bind, APIPort: &port})
	require.NoError(err)
	require.NoError(cfg.PrepareServerKey())
	srv := NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler))
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), get.Header().Get("ETag"), "")
	require.Equal(http.StatusOK, patch.Code, patch.Body.String())
	for _, response := range []*httptest.ResponseRecorder{get, patch} {
		var body SettingsResponse
		require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
		settings := settingsByKey(body.Settings)
		assert.Equal(bind, *settings["server.bind_addr"].Value.String)
		assert.Equal(port, *settings["server.api_port"].Value.Integer)
	}
	snapshot, err := config.ReadConfigFile(cfg.ConfigFilePath())
	require.NoError(err)
	saved, err := config.LoadConfigFile(snapshot, home)
	require.NoError(err)
	assert.Equal("dark", saved.Web.Theme)
	assert.Equal("127.0.0.1", saved.Server.BindAddr)
	assert.Equal(8080, saved.Server.APIPort)
}
