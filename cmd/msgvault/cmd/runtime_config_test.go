package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
)

func TestRuntimeRemoteEnvironmentUsesSelectedFile(t *testing.T) { //nolint:paralleltest // process environment
	assert := assert.New(t)
	require := require.New(t)
	keyFile := filepath.Join(t.TempDir(), "remote-key")
	require.NoError(fileutil.SecureWriteFile(keyFile, []byte("remote-file-key\n"), 0o600))
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("remote-file-key", r.Header.Get("X-Api-Key"))
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": api.APISchemaVersion})
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("MSGVAULT_REMOTE_URL", daemon.URL)
	t.Setenv("MSGVAULT_REMOTE_API_KEY_FILE", keyFile)
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "true")
	t.Setenv("MSGVAULT_API_KEY_FILE", filepath.Join(t.TempDir(), "unused-missing-server-key"))
	cfg, err := config.Load("", t.TempDir())
	require.NoError(err)
	ctx := withStoreResolverConfig(t, cfg)
	client, info, err := OpenHTTPStore(ctx)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	assert.Equal(HTTPStoreConfiguredRemote, info.Kind)
	assert.Equal(daemon.URL, info.URL)
	_, err = client.Health(ctx)
	require.NoError(err)
}

func TestRuntimeLocalIgnoresUnusedRemoteSecret(t *testing.T) { //nolint:paralleltest // process environment and invocation options
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_REMOTE_URL", "https://archive.example.test")
	t.Setenv("MSGVAULT_REMOTE_API_KEY_FILE", filepath.Join(t.TempDir(), "unused-missing-remote-key"))
	cfg, err := config.Load("", t.TempDir())
	require.NoError(err)
	disabled := false
	cfg.Server.DaemonAutoStart = &disabled
	ctx := withStoreResolverConfig(t, cfg)
	invocationFromContext(ctx).options.useLocal = true
	_, _, err = OpenHTTPStore(ctx)
	require.Error(err)
	assert.NotContains(err.Error(), "remote API key")
	assert.NotContains(err.Error(), "credential file")
}

func TestExportTokenKeepsMountedKeyOutOfSavedConfig(t *testing.T) { //nolint:paralleltest // command flags are legacy globals
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	keyFile := filepath.Join(home, "key")
	require.NoError(fileutil.SecureWriteFile(keyFile, []byte("mounted-export-key"), 0o600))
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte("[remote]\nurl = \"http://old.example.test\"\napi_key_file = \"key\"\nallow_insecure = true\n"), 0o600))
	cfg, err := config.Load(path, home)
	require.NoError(err)
	require.NoError(os.MkdirAll(cfg.TokensDir(), 0o700))
	require.NoError(fileutil.SecureWriteFile(filepath.Join(cfg.TokensDir(), "account@example.test.json"), []byte(`{"access_token":"synthetic-access-token"}`), 0o600))
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("mounted-export-key", r.Header.Get("X-Api-Key"))
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(daemon.Close)
	previousURL, previousKey, previousAllow := exportTokenTo, exportTokenAPIKey, exportAllowInsecure
	exportTokenTo, exportTokenAPIKey, exportAllowInsecure = daemon.URL, "", false
	t.Cleanup(func() {
		exportTokenTo, exportTokenAPIKey, exportAllowInsecure = previousURL, previousKey, previousAllow
	})
	command := &cobra.Command{}
	command.Flags().String("api-key", "", "")
	command.SetContext(withStoreResolverConfig(t, cfg))
	require.NoError(runExportToken(command, []string{"account@example.test"}))
	saved, err := os.ReadFile(path)
	require.NoError(err)
	assert.NotContains(string(saved), "mounted-export-key")
	assert.Contains(string(saved), "api_key_file")
	assert.Contains(string(saved), daemon.URL)
}
