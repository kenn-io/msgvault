package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
)

func TestRuntimeLoopbackInterfaceAllowsUnrelatedConfigEdit(t *testing.T) { //nolint:paralleltest // process environment
	require := require.New(t)
	interfaces, err := net.Interfaces()
	require.NoError(err)
	var loopback string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			loopback = iface.Name
			break
		}
	}
	if loopback == "" {
		t.Skip("no active loopback interface")
	}
	t.Setenv("MSGVAULT_BIND_ADDR", "iface:"+loopback)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	require.NoError(err, "a loopback interface does not require an API key")
	contents, err := os.ReadFile(path)
	require.NoError(err)
	assert.NotContains(t, string(contents), "bind_addr", "runtime interface resolution must not seed a bind address")
}

// Returning early for an absent config must not discard deployment controls.
func TestRuntimeEnvironmentWithoutConfig(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	assert := assert.New(t)
	home := t.TempDir()
	t.Setenv("MSGVAULT_BIND_ADDR", "0.0.0.0")
	t.Setenv("MSGVAULT_API_PORT", "8480")
	t.Setenv("MSGVAULT_BACKUP_REPO", filepath.Join(home, "backups"))
	t.Setenv("MSGVAULT_REMOTE_URL", "https://archive.example.test")
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "false")
	t.Setenv("MSGVAULT_CORS_ORIGINS", "https://archive.example.test, https://archive2.example.test")
	t.Setenv("MSGVAULT_TRUSTED_PROXIES", "127.0.0.1, ::1")
	t.Setenv("MSGVAULT_CORS_CREDENTIALS", "true")
	cfg, err := Load("", home)
	require.NoError(t, err)
	assert.Equal("0.0.0.0", cfg.Server.BindAddr)
	assert.Equal(8480, cfg.Server.APIPort)
	assert.Equal(filepath.Join(home, "backups"), cfg.Backup.Repo)
	assert.Equal("https://archive.example.test", cfg.Remote.URL)
	assert.False(cfg.Remote.AllowInsecure)
	assert.Equal([]string{"https://archive.example.test", "https://archive2.example.test"}, cfg.Server.CORSOrigins)
	assert.Equal([]string{"127.0.0.1", "::1"}, cfg.Server.TrustedProxies)
	assert.True(cfg.Server.CORSCredentials)
	_, err = os.Stat(filepath.Join(home, "config.toml"))
	assert.ErrorIs(err, os.ErrNotExist, "environment-only loading must not seed config")
}

func TestRuntimeEnvironmentBeatsConfig(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte(`[server]
bind_addr = "0.0.0.0"
api_port = 8080
cors_origins = ["https://old.example.test"]
[remote]
url = "https://old.example.test"
allow_insecure = true
`), 0o600))
	t.Setenv("MSGVAULT_BIND_ADDR", "127.0.0.1")
	t.Setenv("MSGVAULT_API_PORT", "8181")
	t.Setenv("MSGVAULT_CORS_ORIGINS", "")
	t.Setenv("MSGVAULT_REMOTE_URL", "https://new.example.test")
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "false")
	cfg, err := Load(path, home)
	require.NoError(err)
	assert.Equal("127.0.0.1", cfg.Server.BindAddr)
	assert.Equal(8181, cfg.Server.APIPort)
	assert.Empty(cfg.Server.CORSOrigins)
	assert.Equal("https://new.example.test", cfg.Remote.URL)
	assert.False(cfg.Remote.AllowInsecure)
}

func TestRuntimeEnvironmentOverridesDoNotPersistDuringSave(t *testing.T) { //nolint:paralleltest // process environment
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte(`[server]
bind_addr = "127.0.0.1"
allow_insecure = false
`), 0o600))
	t.Setenv("MSGVAULT_BIND_ADDR", "0.0.0.0")
	t.Setenv("MSGVAULT_ALLOW_INSECURE", "true")

	cfg, err := Load(path, home)
	require.NoError(err)
	assert.Equal("0.0.0.0", cfg.Server.BindAddr)
	assert.True(cfg.Server.AllowInsecure)
	cfg.Sync.RateLimitQPS = 17
	require.NoError(cfg.Save())

	require.NoError(os.Unsetenv("MSGVAULT_BIND_ADDR"))
	require.NoError(os.Unsetenv("MSGVAULT_ALLOW_INSECURE"))
	reloaded, err := Load(path, home)
	require.NoError(err)
	assert.Equal("127.0.0.1", reloaded.Server.BindAddr)
	assert.False(reloaded.Server.AllowInsecure)
	assert.Equal(17, reloaded.Sync.RateLimitQPS)
}

func TestRuntimeFlagOverridesDoNotPersistDuringSave(t *testing.T) { //nolint:paralleltest // process environment
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte(`[server]
bind_addr = "127.0.0.1"
api_port = 8080
`), 0o600))
	t.Setenv("MSGVAULT_BIND_ADDR", "")
	t.Setenv("MSGVAULT_API_PORT", "invalid")
	t.Setenv("MSGVAULT_ALLOW_INSECURE", "false")
	bindAddr, apiPort := "127.0.0.2", 8181
	cfg, err := LoadWithOverrides(path, home, RuntimeOverrides{BindAddr: &bindAddr, APIPort: &apiPort})
	require.NoError(err)
	assert.Equal(bindAddr, cfg.Server.BindAddr)
	assert.Equal(apiPort, cfg.Server.APIPort)
	cfg.Sync.RateLimitQPS = 17
	require.NoError(cfg.Save())

	require.NoError(os.Unsetenv("MSGVAULT_BIND_ADDR"))
	require.NoError(os.Unsetenv("MSGVAULT_API_PORT"))
	reloaded, err := Load(path, home)
	require.NoError(err)
	assert.Equal("127.0.0.1", reloaded.Server.BindAddr)
	assert.Equal(8080, reloaded.Server.APIPort)
	assert.Equal(17, reloaded.Sync.RateLimitQPS)
}

func TestRuntimeEnvironmentRejectsInvalidValues(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	for _, tc := range []struct{ name, value string }{
		{"MSGVAULT_BIND_ADDR", ""},
		{"MSGVAULT_API_PORT", ""},
		{"MSGVAULT_API_PORT", "65536"},
		{"MSGVAULT_API_PORT", "-1"},
		{"MSGVAULT_API_PORT", "not-a-port"},
		{"MSGVAULT_REMOTE_ALLOW_INSECURE", ""},
		{"MSGVAULT_REMOTE_ALLOW_INSECURE", "maybe"},
		{"MSGVAULT_CORS_CREDENTIALS", ""},
		{"MSGVAULT_TRUSTED_PROXIES", "not-an-ip"},
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			_, err := Load("", t.TempDir())
			require.Error(t, err)
		})
	}
}

func TestRuntimeSecretEnvironmentDoesNotPersist(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte(`[server]
api_key = "legacy-server-key"
[remote]
api_key = "legacy-remote-key"
`), 0o600))
	t.Setenv("MSGVAULT_API_KEY", "runtime-server-key")
	t.Setenv("MSGVAULT_REMOTE_API_KEY", "runtime-remote-key")
	cfg, err := Load(path, home)
	require.NoError(err)
	require.NoError(cfg.ResolveServerKey())
	require.NoError(cfg.ResolveRemoteKey())
	assert.Equal("runtime-server-key", cfg.Server.AuthenticationKey())
	assert.Equal("runtime-remote-key", cfg.Remote.AuthenticationKey())
	require.NoError(cfg.Save())
	saved, err := os.ReadFile(path)
	require.NoError(err)
	assert.NotContains(string(saved), "runtime-server-key")
	assert.NotContains(string(saved), "runtime-remote-key")
	assert.Contains(string(saved), "legacy-server-key")
	assert.Contains(string(saved), "legacy-remote-key")
}

func TestRuntimeUnusedSecretsAreResolvedOnDemand(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte(`[server]
api_key_file = "missing-server-key"
[remote]
api_key_file = "missing-remote-key"
`), 0o600))
	cfg, err := Load(path, home)
	require.NoError(err, "loading an unused destination must not read its secret")
	require.Error(cfg.ResolveServerKey())
	assert.Error(cfg.ResolveRemoteKey())
}

func TestRuntimeSecretFilesResolveRelativeToExplicitConfig(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(filepath.Join(home, "key"), []byte("file-key\n"), 0o600))
	require.NoError(fileutil.SecureWriteFile(path, []byte(`[server]
api_key_file = "key"
[remote]
api_key_file = "key"
[integrations.docbank]
api_key_file = "key"
api_key_env = "MISSING_DOCBANK_KEY"
`), 0o600))
	cfg, err := Load(path, "")
	require.NoError(err)
	require.NoError(cfg.ResolveServerKey())
	require.NoError(cfg.ResolveRemoteKey())
	assert.Equal("file-key", cfg.Server.AuthenticationKey())
	assert.Equal("file-key", cfg.Remote.AuthenticationKey())
	key, err := cfg.Integrations.Docbank.ResolveAPIKey()
	require.NoError(err)
	assert.Equal("file-key", key)
	require.NoError(fileutil.SecureWriteFile(filepath.Join(home, "key"), []byte("replacement-key"), 0o600))
	key, err = cfg.Integrations.Docbank.ResolveAPIKey()
	require.NoError(err)
	assert.Equal("replacement-key", key, "request-time consumers reread mounted keys")
}

func TestRuntimeWinningFlagsIgnoreMalformedEnvironment(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	t.Setenv("MSGVAULT_BIND_ADDR", "")
	t.Setenv("MSGVAULT_API_PORT", "invalid")
	bind, port := "127.0.0.1", 8181
	cfg, err := LoadWithOverrides("", t.TempDir(), RuntimeOverrides{BindAddr: &bind, APIPort: &port})
	require.NoError(t, err)
	assert.Equal(t, bind, cfg.Server.BindAddr)
	assert.Equal(t, port, cfg.Server.APIPort)
}

func FuzzRuntimeWinningPort(f *testing.F) {
	f.Add(uint16(0))
	f.Add(uint16(8181))
	f.Add(uint16(65535))
	f.Fuzz(func(t *testing.T, port uint16) {
		t.Setenv("MSGVAULT_API_PORT", "invalid-lower-priority-value")
		want := int(port)
		cfg, err := LoadWithOverrides("", t.TempDir(), RuntimeOverrides{APIPort: &want})
		require.NoError(t, err)
		assert.Equal(t, want, cfg.Server.APIPort)
	})
}

func TestRuntimeLegacyEmptyBindRetainsLoopbackDefault(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, fileutil.SecureWriteFile(path, []byte("[server]\nbind_addr = \"\"\n"), 0o600))
	cfg, err := Load(path, "")
	require.NoError(t, err)
	assert.True(t, cfg.Server.IsLoopback())
}

func TestRuntimeSelectedSecretEnvironmentFailsWithoutFallback(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("MSGVAULT_REMOTE_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte(`[server]
api_key = "legacy-server-key"
[remote]
api_key = "legacy-remote-key"
`), 0o600))
	cfg, err := Load(path, home)
	require.NoError(err)
	require.Error(cfg.ResolveServerKey())
	require.Error(cfg.ResolveRemoteKey())
	assert.Empty(cfg.Server.AuthenticationKey())
	assert.Empty(cfg.Remote.AuthenticationKey())
}

func TestDocbankSecretPrecedenceAndFailures(t *testing.T) { //nolint:paralleltest // credential environment is process-wide
	t.Setenv("MSGVAULT_TEST_DOCBANK_SECRET", "environment-key")
	file := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name      string
		config    DocbankIntegrationConfig
		want      string
		wantError bool
	}{
		{"inline", DocbankIntegrationConfig{APIKey: "inline-key", APIKeyFile: file, APIKeyEnv: "MSGVAULT_TEST_DOCBANK_SECRET"}, "inline-key", false},
		{"selected missing file", DocbankIntegrationConfig{APIKeyFile: file, APIKeyEnv: "MSGVAULT_TEST_DOCBANK_SECRET"}, "", true},
		{"environment", DocbankIntegrationConfig{APIKeyEnv: "MSGVAULT_TEST_DOCBANK_SECRET"}, "environment-key", false},
		{"missing environment", DocbankIntegrationConfig{APIKeyEnv: "MSGVAULT_TEST_MISSING_DOCBANK_SECRET"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.config.ResolveAPIKey()
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRuntimeMissingConfigSnapshotKeepsHome(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	snapshot, err := ReadConfigFile(filepath.Join(home, "config.toml"))
	require.NoError(err)
	cfg, err := LoadConfigFile(snapshot, home)
	require.NoError(err)
	assert.Equal(home, cfg.HomeDir)
	assert.Equal(filepath.Join(home, "tokens"), cfg.TokensDir())
}

func TestRuntimeAgentAccessSnapshotAcceptsEnvironmentCredential(t *testing.T) { //nolint:paralleltest // credential environment is process-wide
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_API_KEY", "runtime-agent-key")
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(fileutil.SecureWriteFile(path, []byte("[server]\nagent_access = true\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	cfg, err := LoadConfigFile(snapshot, home)
	require.NoError(err)
	require.NoError(cfg.ResolveServerKey())
	assert.Equal("runtime-agent-key", cfg.Server.AuthenticationKey())
	assert.Empty(cfg.Server.APIKey)
}

func TestRuntimeEnsureHomePreservesExistingDirectoryPermissions(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory modes; Windows uses ACLs")
	}
	home := t.TempDir()
	require.NoError(os.Chmod(home, 0o755))
	cfg := NewDefaultConfig()
	cfg.HomeDir = home
	require.NoError(cfg.EnsureHomeDir())
	info, err := os.Stat(home)
	require.NoError(err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestRuntimeEnsureHomeLeavesCustomConfigParentPermissions(t *testing.T) {
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory modes; Windows uses ACLs")
	}
	parent := t.TempDir()
	require.NoError(os.Chmod(parent, 0o777))
	configPath := filepath.Join(parent, "config.toml")
	require.NoError(fileutil.SecureWriteFile(configPath, []byte(""), 0o600))
	cfg, err := Load(configPath, "")
	require.NoError(err)
	require.Equal(parent, cfg.HomeDir)

	require.NoError(cfg.EnsureHomeDir())
	info, err := os.Stat(parent)
	require.NoError(err)
	assert.Equal(t, os.FileMode(0o777), info.Mode().Perm())
}

func TestRuntimeEnsureHomePreservesExplicitHomeWithCustomConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory modes; Windows uses ACLs")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "shared")
	home := filepath.Join(root, "home")
	require.NoError(os.Mkdir(configDir, 0o700))
	require.NoError(os.Chmod(configDir, 0o777))
	require.NoError(os.Mkdir(home, 0o700))
	require.NoError(os.Chmod(home, 0o777))
	configPath := filepath.Join(configDir, "config.toml")
	require.NoError(fileutil.SecureWriteFile(configPath, []byte("[web]\ntheme = \"dark\"\n"), 0o600))

	cfg, err := Load(configPath, home)
	require.NoError(err)
	require.NoError(cfg.EnsureHomeDir())

	homeInfo, err := os.Stat(home)
	require.NoError(err)
	assert.Equal(os.FileMode(0o777), homeInfo.Mode().Perm())
	configInfo, err := os.Stat(configDir)
	require.NoError(err)
	assert.Equal(os.FileMode(0o777), configInfo.Mode().Perm())
}

func TestServerKeySurvivesHomeReplacementWithPersistentData(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	data := t.TempDir()
	var firstKey string
	for range 2 {
		home := t.TempDir()
		path := filepath.Join(home, "config.toml")
		require.NoError(fileutil.SecureWriteFile(path,
			[]byte(fmt.Sprintf("[data]\ndata_dir = %q\n[server]\nbind_addr = '0.0.0.0'\n", filepath.ToSlash(data))), 0o600))
		cfg, err := Load(path, home)
		require.NoError(err)
		require.NoError(cfg.PrepareServerKey())
		persisted, err := os.ReadFile(filepath.Join(data, "tokens", "server-api-key"))
		require.NoError(err)
		assert.Equal(cfg.Server.AuthenticationKey()+"\n", string(persisted))
		if firstKey == "" {
			firstKey = cfg.Server.AuthenticationKey()
		} else {
			assert.Equal(firstKey, cfg.Server.AuthenticationKey())
		}
	}
}

func TestRuntimeListsIgnoreEmptyEntries(t *testing.T) { //nolint:paralleltest // process environment
	t.Setenv("MSGVAULT_CORS_ORIGINS", " https://ui.example.test, ,")
	t.Setenv("MSGVAULT_TRUSTED_PROXIES", ",127.0.0.1, , ::1,")
	cfg, err := Load("", t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, []string{"https://ui.example.test"}, cfg.Server.CORSOrigins)
	assert.Equal(t, []string{"127.0.0.1", "::1"}, cfg.Server.TrustedProxies)
}
