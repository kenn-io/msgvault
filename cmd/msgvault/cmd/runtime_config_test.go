package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
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
	previousURL, previousKey, previousAllow := exportTokenTo, exportTokenAPIKey, exportAllowInsecure
	t.Cleanup(func() {
		exportTokenTo, exportTokenAPIKey, exportAllowInsecure = previousURL, previousKey, previousAllow
	})
	for _, tt := range []struct {
		name              string
		explicitURL       bool
		explicitInsecure  *bool
		persistedInsecure bool
		tls               bool
		wantInsecure      bool
	}{
		{name: "explicit URL matches environment", explicitURL: true},
		{name: "explicit allow-insecure matches environment", explicitInsecure: new(true), wantInsecure: true},
		{name: "explicit false overrides environment and config", explicitInsecure: new(false), persistedInsecure: true, tls: true},
		{name: "environment only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			home := t.TempDir()
			keyFile := filepath.Join(home, "key")
			require.NoError(fileutil.SecureWriteFile(keyFile, []byte("mounted-export-key"), 0o600))
			path := filepath.Join(home, "config.toml")
			require.NoError(fileutil.SecureWriteFile(path, fmt.Appendf(nil, "[remote]\nurl = \"http://old.example.test\"\napi_key_file = \"key\"\nallow_insecure = %t\n", tt.persistedInsecure), 0o600))
			daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("mounted-export-key", r.Header.Get("X-Api-Key"))
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(daemon.Close)
			if tt.tls {
				daemon.StartTLS()
			} else {
				daemon.Start()
			}
			t.Setenv("MSGVAULT_REMOTE_URL", daemon.URL)
			t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "true")
			cfg, err := config.Load(path, home)
			require.NoError(err)
			require.NoError(os.MkdirAll(cfg.TokensDir(), 0o700))
			require.NoError(fileutil.SecureWriteFile(filepath.Join(cfg.TokensDir(), "account@example.test.json"), []byte(`{"access_token":"synthetic-access-token"}`), 0o600))
			exportTokenTo, exportTokenAPIKey = "", ""
			wantURL := "http://old.example.test"
			if tt.explicitURL {
				exportTokenTo, wantURL = daemon.URL, daemon.URL
			}
			command := &cobra.Command{}
			command.Flags().String("api-key", "", "")
			command.Flags().BoolVar(&exportAllowInsecure, "allow-insecure", false, "")
			if tt.explicitInsecure != nil {
				require.NoError(command.Flags().Set("allow-insecure", strconv.FormatBool(*tt.explicitInsecure)))
			}
			command.SetContext(withStoreResolverConfig(t, cfg))
			require.NoError(runExportTokenWithClient(command, []string{"account@example.test"}, daemon.Client()))
			snapshot, err := config.ReadConfigFile(path)
			require.NoError(err)
			saved, err := config.LoadConfigFile(snapshot, home)
			require.NoError(err)
			assert.Equal(wantURL, saved.Remote.URL)
			assert.Equal(tt.wantInsecure, saved.Remote.AllowInsecure)
			assert.Equal(keyFile, saved.Remote.APIKeyFile)
			assert.Empty(saved.Remote.APIKey)
		})
	}
}

func TestExportTokenExplicitFalseRequiresHTTPS(t *testing.T) { //nolint:paralleltest // command globals and process environment
	for _, source := range []string{"environment", "config"} {
		t.Run(source, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var requests atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(daemon.Close)
			t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "true")
			if source == "config" {
				require.NoError(os.Unsetenv("MSGVAULT_REMOTE_ALLOW_INSECURE"))
			}
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			require.NoError(fileutil.SecureWriteFile(path, fmt.Appendf(nil,
				"[remote]\nurl = %q\napi_key = 'synthetic-remote-key'\nallow_insecure = %t\n", daemon.URL, source == "config"), 0o600))
			cfg, err := config.Load(path, home)
			require.NoError(err)
			require.NoError(fileutil.SecureMkdirAll(cfg.TokensDir(), 0o700))
			require.NoError(fileutil.SecureWriteFile(filepath.Join(cfg.TokensDir(), "account@example.test.json"), []byte(`{"access_token":"synthetic-token"}`), 0o600))
			previousURL, previousKey, previousAllow := exportTokenTo, exportTokenAPIKey, exportAllowInsecure
			t.Cleanup(func() {
				exportTokenTo, exportTokenAPIKey, exportAllowInsecure = previousURL, previousKey, previousAllow
			})
			exportTokenTo, exportTokenAPIKey = "", ""
			command := &cobra.Command{}
			command.Flags().BoolVar(&exportAllowInsecure, "allow-insecure", false, "")
			require.NoError(command.Flags().Set("allow-insecure", "false"))
			command.SetContext(withStoreResolverConfig(t, cfg))
			err = runExportToken(command, []string{"account@example.test"})
			require.ErrorContains(err, "HTTPS required")
			assert.Zero(requests.Load(), "explicit false must prevent the token upload over HTTP")
		})
	}
}
