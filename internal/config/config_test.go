package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestServerConfigDefaults(t *testing.T) {
	// Create a temp dir without a config file
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	cfg, err := Load("", "")
	requirepkg.NoError(t, err, "Load()")

	// Check server defaults
	assertpkg.Equal(t, 8080, cfg.Server.APIPort)
	assertpkg.Empty(t, cfg.Server.APIKey)
}

func TestAccountScheduleEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	cfg, err := Load("", "")
	requirepkg.NoError(t, err, "Load()")

	assertpkg.Empty(t, cfg.Accounts)

	scheduled := cfg.ScheduledAccounts()
	assertpkg.Empty(t, scheduled)
}

func TestLoadWithServerConfig(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[server]
api_port = 9090
api_key = "test-secret-key"

[[accounts]]
email = "test@gmail.com"
schedule = "0 2 * * *"
enabled = true

[[accounts]]
email = "other@gmail.com"
schedule = "0 3 * * *"
enabled = false
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "WriteFile()")

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load()")

	// Check server config
	assert.Equal(9090, cfg.Server.APIPort)
	assert.Equal("test-secret-key", cfg.Server.APIKey)

	// Check accounts
	require.Len(cfg.Accounts, 2)

	assert.Equal("test@gmail.com", cfg.Accounts[0].Email)
	assert.Equal("0 2 * * *", cfg.Accounts[0].Schedule)
	assert.True(cfg.Accounts[0].Enabled)
}

func TestScheduledAccounts(t *testing.T) {
	assert := assertpkg.New(t)
	cfg := &Config{
		Accounts: []AccountSchedule{
			{Email: "enabled@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "disabled@gmail.com", Schedule: "0 3 * * *", Enabled: false},
			{Email: "noschedule@gmail.com", Schedule: "", Enabled: true},
			{Email: "both@gmail.com", Schedule: "0 4 * * *", Enabled: true},
		},
	}

	scheduled := cfg.ScheduledAccounts()

	requirepkg.Len(t, scheduled, 2)

	// Should contain only enabled accounts with schedules
	emails := make(map[string]bool)
	for _, acc := range scheduled {
		emails[acc.Email] = true
	}

	assert.True(emails["enabled@gmail.com"], "ScheduledAccounts() missing enabled@gmail.com")
	assert.True(emails["both@gmail.com"], "ScheduledAccounts() missing both@gmail.com")
	assert.False(emails["disabled@gmail.com"], "ScheduledAccounts() should not include disabled account")
	assert.False(emails["noschedule@gmail.com"], "ScheduledAccounts() should not include account without schedule")
}

func TestGetAccountSchedule(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountSchedule{
			{Email: "test@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "other@gmail.com", Schedule: "0 3 * * *", Enabled: false},
		},
	}

	tests := []struct {
		email     string
		wantNil   bool
		wantSched string
	}{
		{"test@gmail.com", false, "0 2 * * *"},
		{"other@gmail.com", false, "0 3 * * *"},
		{"notfound@gmail.com", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			acc := cfg.GetAccountSchedule(tt.email)
			if tt.wantNil {
				assertpkg.Nil(t, acc, "GetAccountSchedule(%q)", tt.email)
				return
			}
			requirepkg.NotNil(t, acc, "GetAccountSchedule(%q)", tt.email)
			assertpkg.Equal(t, tt.wantSched, acc.Schedule, "GetAccountSchedule(%q).Schedule", tt.email)
		})
	}
}

func TestGetAccountScheduleReturnsCopy(t *testing.T) {
	assert := assertpkg.New(t)
	cfg := &Config{
		Accounts: []AccountSchedule{
			{Email: "test@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}

	// Get a reference and mutate it
	acc := cfg.GetAccountSchedule("test@gmail.com")
	requirepkg.NotNil(t, acc, "GetAccountSchedule returned nil")

	// Mutate the returned copy
	acc.Schedule = "modified"
	acc.Enabled = false
	acc.Email = "hacked@gmail.com"

	// Original config must be unchanged
	assert.Equal("0 2 * * *", cfg.Accounts[0].Schedule, "original Schedule (mutation leaked)")
	assert.True(cfg.Accounts[0].Enabled, "original Enabled (mutation leaked)")
	assert.Equal("test@gmail.com", cfg.Accounts[0].Email, "original Email (mutation leaked)")
}

func TestSynctechSMSSourcesConfig(t *testing.T) {
	require := requirepkg.New(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	data := []byte(`
[[synctech_sms.sources]]
name = "pixel"
backend = "drive"
owner_phone = "+15550000001"
folder_id = "drive-folder-id"
google_account = "user@example.com"
schedule = "30 4 * * *"
include_sms = true
include_mms = true
include_calls = true
include_attachments = true
stable_after = "10m"
oauth_app = "personal"
`)
	require.NoError(os.WriteFile(configPath, data, 0o600), "write config")
	cfg, err := Load(configPath, "")
	require.NoError(err, "Load")
	src := cfg.GetSynctechSMSSource("pixel")
	require.NotNil(src, "GetSynctechSMSSource returned nil")
	require.Truef(src.Backend == "drive" && src.OwnerPhone == "+15550000001" && src.FolderID == "drive-folder-id" && src.GoogleAccount == "user@example.com" && src.StableAfter == "10m", "source mismatch: %#v", src)
}

func TestSynctechSMSScheduledSources(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.SynctechSMS.Sources = []SynctechSMSSource{
		{Name: "enabled", Enabled: true, Schedule: "30 4 * * *", OwnerPhone: "+15550000001", Backend: "local", Path: "/tmp/inbox"},
		{Name: "disabled", Enabled: false, Schedule: "30 4 * * *", OwnerPhone: "+15550000002", Backend: "local", Path: "/tmp/inbox"},
		{Name: "unscheduled", Enabled: true, OwnerPhone: "+15550000003", Backend: "local", Path: "/tmp/inbox"},
	}
	got := cfg.ScheduledSynctechSMSSources()
	requirepkg.Truef(t, len(got) == 1 && got[0].Name == "enabled", "ScheduledSynctechSMSSources = %#v", got)
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	requirepkg.NoError(t, err, "failed to get user home dir")

	tests := []struct {
		name        string
		input       string
		expected    string
		unixOnly    bool // skip on Windows (uses Unix-style absolute paths)
		windowsOnly bool // skip on non-Windows (quote stripping is Windows-only)
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "just tilde",
			input:    "~",
			expected: home,
		},
		{
			name:     "tilde with slash and path",
			input:    "~/foo",
			expected: filepath.Join(home, "foo"),
		},
		{
			name:     "tilde with trailing slash only",
			input:    "~/",
			expected: home,
		},
		{
			name:     "tilde user notation not expanded",
			input:    "~user",
			expected: "~user",
		},
		{
			name:     "tilde with double slash",
			input:    "~//foo",
			expected: filepath.Join(home, "foo"),
		},
		{
			name:        "single-quoted path (Windows CMD)",
			input:       `'C:\Users\wesmc\testing'`,
			expected:    `C:\Users\wesmc\testing`,
			windowsOnly: true,
		},
		{
			name:        "double-quoted path (Windows CMD)",
			input:       `"C:\Users\wesmc\testing"`,
			expected:    `C:\Users\wesmc\testing`,
			windowsOnly: true,
		},
		{
			name:        "single-quoted tilde path",
			input:       "'~/custom-data'",
			expected:    filepath.Join(home, "custom-data"),
			windowsOnly: true,
		},
		{
			name:     "mismatched quotes not stripped",
			input:    `'C:\Users\wesmc"`,
			expected: `'C:\Users\wesmc"`,
		},
		{
			name:     "single char not stripped",
			input:    "'",
			expected: "'",
		},
		{
			name:     "absolute path unchanged",
			input:    "/var/log/test",
			expected: "/var/log/test",
			unixOnly: true,
		},
		{
			name:     "relative path unchanged",
			input:    "relative/path",
			expected: "relative/path",
		},
		{
			name:     "tilde in middle not expanded",
			input:    "/home/~user/foo",
			expected: "/home/~user/foo",
			unixOnly: true,
		},
		{
			name:     "nested path after tilde",
			input:    "~/foo/bar/baz",
			expected: filepath.Join(home, "foo/bar/baz"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unixOnly && runtime.GOOS == "windows" {
				t.Skip("skipping Unix-specific path test on Windows")
			}
			if tt.windowsOnly && runtime.GOOS != "windows" {
				t.Skip("skipping Windows-specific path test on non-Windows")
			}
			assertpkg.Equal(t, tt.expected, expandPath(tt.input), "expandPath(%q)", tt.input)
		})
	}
}

func TestLoadEmptyPath(t *testing.T) {
	assert := assertpkg.New(t)
	// Use a temp directory as MSGVAULT_HOME
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	// Load with empty path should use defaults
	cfg, err := Load("", "")
	requirepkg.NoError(t, err, "Load(\"\")")

	// Verify default values
	assert.Equal(tmpDir, cfg.HomeDir)
	assert.Equal(tmpDir, cfg.Data.DataDir)
	assert.Equal(5, cfg.Sync.RateLimitQPS)

	// DatabaseDSN should return default path
	expectedDB := filepath.Join(tmpDir, "msgvault.db")
	assert.Equal(expectedDB, cfg.DatabaseDSN())
}

func TestLoadWithConfigFile(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Use a temp directory as MSGVAULT_HOME
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	// Create a config file with custom values
	configPath := filepath.Join(tmpDir, "config.toml")
	configContent := `
[data]
data_dir = "~/custom/data"

[oauth]
client_secrets = "~/secrets/client.json"

[sync]
rate_limit_qps = 10
`
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0o644), "failed to write config file")

	cfg, err := Load("", "")
	require.NoError(err, "Load(\"\")")

	home, err := os.UserHomeDir()
	require.NoError(err, "failed to get user home dir")

	// Verify paths were expanded
	expectedDataDir := filepath.Join(home, "custom/data")
	assert.Equal(expectedDataDir, cfg.Data.DataDir)

	expectedSecrets := filepath.Join(home, "secrets/client.json")
	assert.Equal(expectedSecrets, cfg.OAuth.ClientSecrets)

	assert.Equal(10, cfg.Sync.RateLimitQPS)
}

func TestLoadExplicitPathNotFound(t *testing.T) {
	// When --config explicitly specifies a file that doesn't exist, Load should error
	_, err := Load("/nonexistent/path/config.toml", "")
	requirepkg.Error(t, err, "Load with explicit nonexistent path should return error")
	assertpkg.Contains(t, err.Error(), "config file not found")
}

func TestLoadExplicitPathDerivedHomeDir(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// When --config points to a custom location, HomeDir and DataDir
	// should derive from the config file's parent directory
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Write a minimal config (no data_dir override)
	configContent := `
[oauth]
client_secrets = "/tmp/secret.json"

[sync]
rate_limit_qps = 3
`
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0o644), "failed to write config file")

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load(%q)", configPath)

	assert.Equal(tmpDir, cfg.HomeDir)
	assert.Equal(tmpDir, cfg.Data.DataDir)
	assert.Equal(3, cfg.Sync.RateLimitQPS)

	// Derived paths should use the custom directory
	expectedDB := filepath.Join(tmpDir, "msgvault.db")
	assert.Equal(expectedDB, cfg.DatabaseDSN())
	expectedTokens := filepath.Join(tmpDir, "tokens")
	assert.Equal(expectedTokens, cfg.TokensDir())
}

func TestLoadExplicitPathWithDataDirOverride(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// When config file explicitly sets data_dir, that should take precedence
	tmpDir := t.TempDir()
	customDataDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Use forward slashes in TOML (works cross-platform)
	configContent := `
[data]
data_dir = "` + filepath.ToSlash(customDataDir) + `"
`
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0o644), "failed to write config file")

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load(%q)", configPath)

	// HomeDir should be config file's directory
	assert.Equal(tmpDir, cfg.HomeDir)
	// DataDir should be the explicit override from config.
	// Normalize both sides since TOML preserves forward slashes on Windows.
	assert.Equal(filepath.Clean(customDataDir), filepath.Clean(cfg.Data.DataDir))
}

func TestLoadExplicitPathRelativePaths(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// When --config is used, relative data_dir and client_secrets should
	// resolve against the config file's directory, not the working directory.
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	configContent := `
[data]
data_dir = "data"

[oauth]
client_secrets = "secrets/client.json"
`
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0o644), "failed to write config file")

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load(%q)", configPath)

	expectedDataDir := filepath.Join(tmpDir, "data")
	assert.Equal(expectedDataDir, cfg.Data.DataDir)

	expectedSecrets := filepath.Join(tmpDir, "secrets/client.json")
	assert.Equal(expectedSecrets, cfg.OAuth.ClientSecrets)
}

func TestLoadExplicitPathWithTilde(t *testing.T) {
	require := requirepkg.New(t)
	// Explicit --config with ~ should be expanded before stat
	home, err := os.UserHomeDir()
	require.NoError(err, "failed to get user home dir")

	// Create a config file in a temp subdir of home to test ~ expansion
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte("[sync]\nrate_limit_qps = 7\n"), 0o644), "failed to write config file")

	// Construct a ~ path: replace the home prefix with ~
	if !strings.HasPrefix(tmpDir, home) {
		t.Skip("temp dir is not under home directory, cannot test ~ expansion")
	}
	tildePath := "~" + tmpDir[len(home):] + "/config.toml"

	cfg, err := Load(tildePath, "")
	require.NoError(err, "Load(%q)", tildePath)

	assertpkg.Equal(t, 7, cfg.Sync.RateLimitQPS)
}

func TestLoadConfigFilePath(t *testing.T) {
	// ConfigFilePath should return the actual loaded path, not the default
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	requirepkg.NoError(t, os.WriteFile(configPath, []byte(""), 0o644), "failed to write config file")

	cfg, err := Load(configPath, "")
	requirepkg.NoError(t, err, "Load(%q)", configPath)

	assertpkg.Equal(t, configPath, cfg.ConfigFilePath())
}

func TestDefaultHomeExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	requirepkg.NoError(t, err, "failed to get user home dir")

	t.Setenv("MSGVAULT_HOME", "~/.msgvault")
	expected := filepath.Join(home, ".msgvault")
	assertpkg.Equal(t, expected, DefaultHome())
}

// assertTempDirSecured checks that a temp dir has permissions no more
// permissive than 0700. This is umask-tolerant (stricter is fine).
func assertTempDirSecured(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows uses DACLs, not Unix permission bits
	}
	info, err := os.Stat(dir)
	requirepkg.NoError(t, err, "Stat temp dir")
	got := info.Mode().Perm()
	assertpkg.Zero(t, got&^os.FileMode(0700), "temp dir perm = %04o, has bits beyond 0700 (extra: %04o)", got, got&^0700)
}

func TestMkTempDir(t *testing.T) {
	t.Run("uses system temp when no preferred dirs", func(t *testing.T) {
		dir, err := MkTempDir("test-*")
		requirepkg.NoError(t, err, "MkTempDir failed")
		defer func() { _ = os.RemoveAll(dir) }()

		_, err = os.Stat(dir)
		requirepkg.NoError(t, err, "temp dir does not exist")
		assertTempDirSecured(t, dir)
	})

	t.Run("uses preferred dir when available", func(t *testing.T) {
		preferred := t.TempDir()
		dir, err := MkTempDir("test-*", preferred)
		requirepkg.NoError(t, err, "MkTempDir failed")
		defer func() { _ = os.RemoveAll(dir) }()

		assertpkg.True(t, strings.HasPrefix(dir, preferred), "temp dir %q not under preferred %q", dir, preferred)
		assertTempDirSecured(t, dir)
	})

	t.Run("skips empty preferred dir strings", func(t *testing.T) {
		dir, err := MkTempDir("test-*", "")
		requirepkg.NoError(t, err, "MkTempDir failed")
		defer func() { _ = os.RemoveAll(dir) }()

		// Should have used system temp, not errored
		_, err = os.Stat(dir)
		requirepkg.NoError(t, err, "temp dir does not exist")
	})

	t.Run("falls back to system temp when preferred dir is inaccessible", func(t *testing.T) {
		dir, err := MkTempDir("test-*", "/nonexistent-dir-that-does-not-exist")
		requirepkg.NoError(t, err, "MkTempDir failed")
		defer func() { _ = os.RemoveAll(dir) }()

		// Should have fallen back to system temp
		assertpkg.NotContains(t, dir, "nonexistent", "should not have used nonexistent dir")
	})

	t.Run("falls back to msgvault home when system temp is unavailable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("cannot make system temp dir unwritable on Windows")
		}

		// Create a restricted temp dir so os.MkdirTemp("", ...) fails
		restrictedTmp := t.TempDir()
		requirepkg.NoError(t, os.Chmod(restrictedTmp, 0o500), "chmod failed")
		t.Cleanup(func() { _ = os.Chmod(restrictedTmp, 0o700) })

		// Probe whether the restriction actually works (root and some ACL
		// configurations can still write to 0500 directories). t.TempDir()
		// cannot target a specific parent and fails the test on error, which
		// is exactly the condition this probe needs to observe.
		probe, probeErr := os.MkdirTemp(restrictedTmp, "probe-*") //nolint:usetesting // intentional: probing a restricted parent dir
		if probeErr == nil {
			_ = os.Remove(probe)
			t.Skip("chmod 0500 did not restrict writes (running as root or permissive ACLs)")
		}

		// Point TMPDIR to the restricted dir and MSGVAULT_HOME to a writable dir
		msgvaultHome := t.TempDir()
		t.Setenv("TMPDIR", restrictedTmp)
		t.Setenv("MSGVAULT_HOME", msgvaultHome)

		dir, err := MkTempDir("test-*")
		requirepkg.NoError(t, err, "MkTempDir failed")
		defer func() { _ = os.RemoveAll(dir) }()

		expectedBase := filepath.Join(msgvaultHome, "tmp")
		assertpkg.True(t, strings.HasPrefix(dir, expectedBase), "temp dir %q not under fallback %q", dir, expectedBase)

		// Verify the tmp dir was created with restrictive permissions
		assertTempDirSecured(t, expectedBase)
		assertTempDirSecured(t, dir)
	})
}

func TestLoadBackslashErrorHint(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "invalid escape (backslash G)",
			// \G is not a valid TOML escape → "invalid escape" error
			content: "[data]\ndata_dir = \"C:\\Games\\msgvault\"\n",
		},
		{
			name: "unicode escape (backslash U)",
			// \U is a TOML Unicode escape expecting 8 hex digits → "hexadecimal digits" error
			content: "[data]\ndata_dir = \"C:\\Users\\wesmc\\msgvault\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			tmpDir := t.TempDir()
			t.Setenv("MSGVAULT_HOME", tmpDir)

			configPath := filepath.Join(tmpDir, "config.toml")
			require.NoError(os.WriteFile(configPath, []byte(tt.content), 0o644), "failed to write config file")

			_, err := Load("", "")
			require.Error(err, "Load should fail on TOML backslash error")

			errMsg := err.Error()
			assert.Contains(errMsg, "hint:", "error should contain hint")
			assert.Contains(errMsg, "forward slashes", "error should mention forward slashes")
			assert.Contains(errMsg, "single quotes", "error should mention single quotes")
		})
	}
}

func TestLoadWithHomeDir(t *testing.T) {
	assert := assertpkg.New(t)
	homeDir := t.TempDir()

	cfg, err := Load("", homeDir)
	requirepkg.NoError(t, err, "Load failed")

	assert.Equal(homeDir, cfg.HomeDir)
	assert.Equal(homeDir, cfg.Data.DataDir)

	// Derived paths should use the home directory
	expectedDB := filepath.Join(homeDir, "msgvault.db")
	assert.Equal(expectedDB, cfg.DatabaseDSN())
	expectedTokens := filepath.Join(homeDir, "tokens")
	assert.Equal(expectedTokens, cfg.TokensDir())
}

func TestLoadWithHomeDirReadsConfig(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// --home should load config.toml from that directory
	homeDir := t.TempDir()
	configPath := filepath.Join(homeDir, "config.toml")
	configContent := `[sync]
rate_limit_qps = 42
`
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0o644), "failed to write config file")

	cfg, err := Load("", homeDir)
	require.NoError(err, "Load failed")

	assert.Equal(42, cfg.Sync.RateLimitQPS)
	assert.Equal(homeDir, cfg.HomeDir)
}

func TestLoadWithHomeDirExpandsTilde(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	home, err := os.UserHomeDir()
	require.NoError(err, "failed to get user home dir")

	cfg, err := Load("", "~/custom-data")
	require.NoError(err, "Load failed")

	expected := filepath.Join(home, "custom-data")
	assert.Equal(expected, cfg.HomeDir)
	assert.Equal(expected, cfg.Data.DataDir)
}

// TestLoadDeprecatedMCPEnabled verifies that old config files containing the
// removed mcp_enabled field still load successfully. BurntSushi/toml silently
// ignores unknown keys, so existing configs should not break after the field
// was removed from ServerConfig.
func TestLoadDeprecatedMCPEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[server]
api_port = 9090
mcp_enabled = true
`
	configPath := filepath.Join(tmpDir, "config.toml")
	requirepkg.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644), "WriteFile()")

	cfg, err := Load("", "")
	requirepkg.NoError(t, err, "Load() should succeed with deprecated mcp_enabled")

	assertpkg.Equal(t, 9090, cfg.Server.APIPort)
}

func TestNewDefaultConfig(t *testing.T) {
	// Use a temp directory as MSGVAULT_HOME
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	cfg := NewDefaultConfig()

	assertpkg.Equal(t, tmpDir, cfg.HomeDir)
	assertpkg.Equal(t, tmpDir, cfg.Data.DataDir)
	assertpkg.Equal(t, 5, cfg.Sync.RateLimitQPS)
}

func TestSaveAndLoad_RoundTrip(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()

	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir
	cfg.OAuth.ClientSecrets = filepath.Join(tmpDir, "secrets.json")
	cfg.Sync.RateLimitQPS = 10
	cfg.Server.APIPort = 9090
	cfg.Server.APIKey = "my-server-key"
	cfg.Remote.URL = "http://nas:8080"
	cfg.Remote.APIKey = "my-remote-key"
	cfg.Remote.AllowInsecure = true
	cfg.Accounts = []AccountSchedule{
		{Email: "user@gmail.com", Schedule: "0 2 * * *", Enabled: true},
	}

	require.NoError(cfg.Save(), "Save()")

	// Load it back
	loaded, err := Load(cfg.ConfigFilePath(), "")
	require.NoError(err, "Load()")

	// Verify all fields survived the round trip
	assert.Equal(cfg.OAuth.ClientSecrets, loaded.OAuth.ClientSecrets)
	assert.Equal(10, loaded.Sync.RateLimitQPS)
	assert.Equal(9090, loaded.Server.APIPort)
	assert.Equal("my-server-key", loaded.Server.APIKey)
	assert.Equal("http://nas:8080", loaded.Remote.URL)
	assert.Equal("my-remote-key", loaded.Remote.APIKey)
	assert.True(loaded.Remote.AllowInsecure)
	require.Len(loaded.Accounts, 1)
	assert.Equal("user@gmail.com", loaded.Accounts[0].Email)
}

func TestSave_CreatesFileWithSecurePermissions(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir

	requirepkg.NoError(t, cfg.Save(), "Save()")

	info, err := os.Stat(cfg.ConfigFilePath())
	requirepkg.NoError(t, err, "Stat config")

	// Should have no group/other permissions (0600 or stricter)
	// Windows doesn't support Unix file permissions.
	if runtime.GOOS != "windows" {
		assertpkg.Zero(t, info.Mode().Perm()&0077, "config perm = %04o, want no group/other access", info.Mode().Perm())
	}
}

func TestSave_TightensWeakPermissions(t *testing.T) {
	require := requirepkg.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions not supported on Windows")
	}

	tmpDir := t.TempDir()
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir

	// Pre-create config file with overly permissive mode
	path := cfg.ConfigFilePath()
	require.NoError(os.WriteFile(path, []byte(""), 0644), "WriteFile")

	require.NoError(cfg.Save(), "Save()")

	info, err := os.Stat(path)
	require.NoError(err, "Stat")
	assertpkg.Zero(t, info.Mode().Perm()&0077, "Save should tighten perms: got %04o, want 0600", info.Mode().Perm())
}

func TestSave_FollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}

	t.Run("absolute target", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		tmpDir := t.TempDir()
		targetDir := t.TempDir()
		targetPath := filepath.Join(targetDir, "actual-config.toml")
		linkPath := filepath.Join(tmpDir, "config.toml")

		require.NoError(os.Symlink(targetPath, linkPath), "Symlink")

		cfg := NewDefaultConfig()
		cfg.HomeDir = tmpDir
		cfg.Sync.RateLimitQPS = 77

		require.NoError(cfg.Save(), "Save()")

		linkTarget, err := os.Readlink(linkPath)
		require.NoError(err, "symlink was replaced")
		assert.Equal(targetPath, linkTarget)

		loaded, err := Load(targetPath, "")
		require.NoError(err, "Load target")
		assert.Equal(77, loaded.Sync.RateLimitQPS)
	})

	t.Run("relative target", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		tmpDir := t.TempDir()
		// Create subdir for the actual file
		subDir := filepath.Join(tmpDir, "real")
		require.NoError(os.Mkdir(subDir, 0700), "Mkdir")
		targetPath := filepath.Join(subDir, "config.toml")
		linkPath := filepath.Join(tmpDir, "config.toml")

		// Relative symlink: config.toml → real/config.toml
		require.NoError(os.Symlink("real/config.toml", linkPath), "Symlink")

		cfg := NewDefaultConfig()
		cfg.HomeDir = tmpDir
		cfg.Sync.RateLimitQPS = 88

		require.NoError(cfg.Save(), "Save()")

		// Symlink must still be intact
		linkTarget, err := os.Readlink(linkPath)
		require.NoError(err, "symlink was replaced")
		assert.Equal("real/config.toml", linkTarget)

		// Target file should contain the saved config
		loaded, err := Load(targetPath, "")
		require.NoError(err, "Load target")
		assert.Equal(88, loaded.Sync.RateLimitQPS)
	})
}

func TestSave_FailurePreservesExisting(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("cannot make directory unwritable on Windows")
	}

	tmpDir := t.TempDir()

	// Save initial valid config
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir
	cfg.Sync.RateLimitQPS = 5
	require.NoError(cfg.Save(), "initial Save")

	// Read back original content
	originalBytes, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err, "ReadFile")

	// Make directory unwritable so CreateTemp fails
	require.NoError(os.Chmod(tmpDir, 0500), "Chmod")
	t.Cleanup(func() { _ = os.Chmod(tmpDir, 0700) })

	// Probe whether the restriction actually works
	probe, probeErr := os.CreateTemp(tmpDir, "probe-*")
	if probeErr == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("chmod 0500 did not restrict writes (running as root)")
	}

	// Save should fail
	cfg.Sync.RateLimitQPS = 99
	require.Error(cfg.Save(), "Save should fail when directory is unwritable")

	// Restore permissions to verify state
	require.NoError(os.Chmod(tmpDir, 0700), "Chmod restore")

	// Original config should be intact
	currentBytes, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err, "ReadFile")
	assert.Equal(string(originalBytes), string(currentBytes), "config file was corrupted after failed Save")

	// No temp files should be left behind
	entries, err := os.ReadDir(tmpDir)
	require.NoError(err, "ReadDir")
	for _, e := range entries {
		assert.False(strings.HasPrefix(e.Name(), ".config-"), "leftover temp file: %s", e.Name())
	}
}

func TestSave_OverwritesExisting(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()

	// Save initial config
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir
	cfg.Sync.RateLimitQPS = 5
	require.NoError(cfg.Save(), "first Save()")

	// Update and save again
	cfg.Sync.RateLimitQPS = 42
	require.NoError(cfg.Save(), "second Save()")

	// Load and verify the update took effect
	loaded, err := Load(cfg.ConfigFilePath(), "")
	require.NoError(err, "Load()")
	assertpkg.Equal(t, 42, loaded.Sync.RateLimitQPS)
}

func TestOAuthConfig_ClientSecretsFor(t *testing.T) {
	tests := []struct {
		name    string
		config  OAuthConfig
		appName string
		want    string
		wantErr bool
	}{
		{
			name:    "empty name returns default",
			config:  OAuthConfig{ClientSecrets: "/path/to/default.json"},
			appName: "",
			want:    "/path/to/default.json",
		},
		{
			name:    "empty name with no default returns error",
			config:  OAuthConfig{},
			appName: "",
			wantErr: true,
		},
		{
			name: "named app returns its path",
			config: OAuthConfig{
				ClientSecrets: "/path/to/default.json",
				Apps: map[string]OAuthApp{
					"acme": {ClientSecrets: "/path/to/acme.json"},
				},
			},
			appName: "acme",
			want:    "/path/to/acme.json",
		},
		{
			name: "named app not found returns error",
			config: OAuthConfig{
				ClientSecrets: "/path/to/default.json",
				Apps: map[string]OAuthApp{
					"acme": {ClientSecrets: "/path/to/acme.json"},
				},
			},
			appName: "missing",
			wantErr: true,
		},
		{
			name: "named app with empty path returns error",
			config: OAuthConfig{
				Apps: map[string]OAuthApp{
					"acme": {ClientSecrets: ""},
				},
			},
			appName: "acme",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.config.ClientSecretsFor(tt.appName)
			if tt.wantErr {
				assertpkg.Error(t, err, "ClientSecretsFor(%q)", tt.appName)
				return
			}
			requirepkg.NoError(t, err, "ClientSecretsFor(%q)", tt.appName)
			assertpkg.Equal(t, tt.want, got, "ClientSecretsFor(%q)", tt.appName)
		})
	}
}

func TestOAuthConfig_ServiceAccountKeyFor(t *testing.T) {
	cfg := OAuthConfig{
		ServiceAccountKey: "/keys/default.json",
		Apps: map[string]OAuthApp{
			"workspace": {ServiceAccountKey: "/keys/workspace.json"},
			"oauth":     {ClientSecrets: "/secrets/oauth.json"},
		},
	}

	tests := []struct {
		name    string
		appName string
		want    string
	}{
		{"default", "", "/keys/default.json"},
		{"named app", "workspace", "/keys/workspace.json"},
		{"named app without service account", "oauth", ""},
		{"missing app", "missing", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertpkg.Equal(t, tt.want, cfg.ServiceAccountKeyFor(tt.appName), "ServiceAccountKeyFor(%q)", tt.appName)
		})
	}
}

func TestOAuthConfig_HasAnyConfig(t *testing.T) {
	tests := []struct {
		name   string
		config OAuthConfig
		want   bool
	}{
		{
			name:   "empty config",
			config: OAuthConfig{},
			want:   false,
		},
		{
			name:   "default only",
			config: OAuthConfig{ClientSecrets: "/path/to/default.json"},
			want:   true,
		},
		{
			name: "named app only",
			config: OAuthConfig{
				Apps: map[string]OAuthApp{
					"acme": {ClientSecrets: "/path/to/acme.json"},
				},
			},
			want: true,
		},
		{
			name: "named app with empty path",
			config: OAuthConfig{
				Apps: map[string]OAuthApp{
					"acme": {ClientSecrets: ""},
				},
			},
			want: false,
		},
		{
			name:   "default service account only",
			config: OAuthConfig{ServiceAccountKey: "/path/to/service-account.json"},
			want:   true,
		},
		{
			name: "named service account only",
			config: OAuthConfig{
				Apps: map[string]OAuthApp{
					"workspace": {ServiceAccountKey: "/path/to/workspace.json"},
				},
			},
			want: true,
		},
		{
			name: "mixed oauth and service account",
			config: OAuthConfig{
				ClientSecrets: "/path/to/default.json",
				Apps: map[string]OAuthApp{
					"workspace": {ServiceAccountKey: "/path/to/workspace.json"},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertpkg.Equal(t, tt.want, tt.config.HasAnyConfig())
		})
	}
}

func TestLoadWithNamedOAuthApps(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[oauth]
client_secrets = "~/secrets/default.json"

[oauth.apps.acme]
client_secrets = "~/secrets/acme.json"

[oauth.apps.personal]
client_secrets = "/absolute/personal.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "WriteFile")

	cfg, err := Load("", "")
	require.NoError(err, "Load()")

	home, err := os.UserHomeDir()
	require.NoError(err, "UserHomeDir")

	// Default should be expanded
	expectedDefault := filepath.Join(home, "secrets/default.json")
	assert.Equal(expectedDefault, cfg.OAuth.ClientSecrets)

	// Named apps should be expanded
	expectedAcme := filepath.Join(home, "secrets/acme.json")
	acme, ok := cfg.OAuth.Apps["acme"]
	require.True(ok, "Apps[acme] not found")
	assert.Equal(expectedAcme, acme.ClientSecrets)

	// Absolute paths should be unchanged
	personal, ok := cfg.OAuth.Apps["personal"]
	require.True(ok, "Apps[personal] not found")
	assert.Equal("/absolute/personal.json", personal.ClientSecrets)

	// HasAnyConfig should be true
	assert.True(cfg.OAuth.HasAnyConfig())
}

func TestLoadExpandsVectorDBPath(t *testing.T) {
	require := requirepkg.New(t)
	home, err := os.UserHomeDir()
	require.NoError(err, "UserHomeDir")

	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[vector]
enabled = true
db_path = "~/custom/vectors.db"

[vector.embeddings]
endpoint = "http://localhost:8080/v1"
model = "nomic-embed-text-v1.5"
dimension = 768
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0o644), "WriteFile")

	cfg, err := Load("", "")
	require.NoError(err, "Load")

	expected := filepath.Join(home, "custom/vectors.db")
	assertpkg.Equal(t, expected, cfg.Vector.DBPath)
}

func TestLoadResolvesRelativeVectorDBPath(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	configContent := `
[vector]
enabled = true
db_path = "sub/vectors.db"

[vector.embeddings]
endpoint = "http://localhost:8080/v1"
model = "nomic-embed-text-v1.5"
dimension = 768
`
	requirepkg.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o644), "WriteFile")

	cfg, err := Load(configPath, "")
	requirepkg.NoError(t, err, "Load")

	expected := filepath.Join(tmpDir, "sub/vectors.db")
	assertpkg.Equal(t, expected, cfg.Vector.DBPath)
}

// TestLoadReappliesVectorDefaults verifies that a zero-valued numeric
// field in the TOML file (e.g. max_retries = 0) gets normalized back to
// the documented default so users cannot accidentally disable retries or
// timeouts. Preprocess booleans use pointer semantics and are exempt
// from this re-defaulting.
func TestLoadReappliesVectorDefaults(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	configContent := `
[vector]
enabled = true

[vector.embeddings]
endpoint = "http://localhost:8080/v1"
model = "nomic-embed-text"
dimension = 768
max_retries = 0
timeout = "0s"

[vector.preprocess]
strip_signatures = false
`
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0o644), "WriteFile")

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load")

	assert.Equal(3, cfg.Vector.Embeddings.MaxRetries, "MaxRetries should re-default from explicit 0")
	// NOTE: TOML "0s" currently decodes to time.Duration(0); post-decode
	// ApplyDefaults lifts it back to 30s to avoid a hang.
	assert.Positive(cfg.Vector.Embeddings.Timeout, "Timeout should re-default from explicit 0s")
	// Explicit false in the TOML file must survive.
	assert.False(cfg.Vector.Preprocess.StripSignaturesEnabled(), "StripSignaturesEnabled() should be false (user explicitly set)")
	// Omitted sibling stays at default true.
	assert.True(cfg.Vector.Preprocess.StripQuotesEnabled(), "StripQuotesEnabled() should be true (unset → default)")
}

func TestLoadWithNamedOAuthApps_RelativePaths(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()

	configContent := `
[oauth.apps.acme]
client_secrets = "secrets/acme.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "WriteFile")

	// Use explicit --config so relative paths resolve against config dir
	cfg, err := Load(configPath, "")
	require.NoError(err, "Load()")

	expectedAcme := filepath.Join(tmpDir, "secrets/acme.json")
	acme, ok := cfg.OAuth.Apps["acme"]
	require.True(ok, "Apps[acme] not found")
	assertpkg.Equal(t, expectedAcme, acme.ClientSecrets)
}

func TestLoadWithServiceAccountKeysExpandsPaths(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	home, err := os.UserHomeDir()
	require.NoError(err, "UserHomeDir")

	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[oauth]
service_account_key = "~/keys/default-service-account.json"

[oauth.apps.workspace]
service_account_key = "~/keys/workspace-service-account.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "WriteFile")

	cfg, err := Load("", "")
	require.NoError(err, "Load()")

	expectedDefault := filepath.Join(home, "keys/default-service-account.json")
	assert.Equal(expectedDefault, cfg.OAuth.ServiceAccountKey)

	expectedWorkspace := filepath.Join(home, "keys/workspace-service-account.json")
	workspace := cfg.OAuth.Apps["workspace"]
	assert.Equal(expectedWorkspace, workspace.ServiceAccountKey)
}

func TestLoadWithServiceAccountKeysResolvesRelativePaths(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()

	configContent := `
[oauth]
service_account_key = "keys/default-service-account.json"

[oauth.apps.workspace]
service_account_key = "keys/workspace-service-account.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "WriteFile")

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load()")

	expectedDefault := filepath.Join(tmpDir, "keys/default-service-account.json")
	assert.Equal(expectedDefault, cfg.OAuth.ServiceAccountKey)

	expectedWorkspace := filepath.Join(tmpDir, "keys/workspace-service-account.json")
	workspace := cfg.OAuth.Apps["workspace"]
	assert.Equal(expectedWorkspace, workspace.ServiceAccountKey)
}

func TestLoadNamedAppsOnly_NoDefault(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[oauth.apps.acme]
client_secrets = "/path/to/acme.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "WriteFile")

	cfg, err := Load("", "")
	require.NoError(err, "Load()")

	// Default should be empty
	assert.Empty(cfg.OAuth.ClientSecrets)

	// HasAnyConfig should still be true
	assert.True(cfg.OAuth.HasAnyConfig())

	// ClientSecretsFor("") should fail
	_, err = cfg.OAuth.ClientSecretsFor("")
	require.Error(err, "ClientSecretsFor(\"\") should error with no default")

	// ClientSecretsFor("acme") should work
	path, err := cfg.OAuth.ClientSecretsFor("acme")
	require.NoError(err, "ClientSecretsFor(acme)")
	assert.Equal("/path/to/acme.json", path)
}

func TestSave_AllowInsecureRoundTrip(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()

	// Save with AllowInsecure = false (default)
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir
	cfg.Remote.URL = "https://nas:8080"
	cfg.Remote.APIKey = "key"
	require.NoError(cfg.Save(), "Save()")

	loaded, err := Load(cfg.ConfigFilePath(), "")
	require.NoError(err, "Load()")
	assert.False(loaded.Remote.AllowInsecure, "AllowInsecure should be false when not set")

	// Now save with AllowInsecure = true
	cfg.Remote.AllowInsecure = true
	require.NoError(cfg.Save(), "Save()")

	loaded, err = Load(cfg.ConfigFilePath(), "")
	require.NoError(err, "Load()")
	assert.True(loaded.Remote.AllowInsecure, "AllowInsecure should be true after saving with true")
}

func TestMicrosoftConfig(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	configContent := `
[microsoft]
client_id = "test-client-id-123"
tenant_id = "my-tenant"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644))

	cfg, err := Load(configPath, tmpDir)
	require.NoError(err)
	assert.Equal("test-client-id-123", cfg.Microsoft.ClientID)
	assert.Equal("my-tenant", cfg.Microsoft.TenantID)
}

func TestMicrosoftConfig_DefaultTenant(t *testing.T) {
	cfg := NewDefaultConfig()
	assertpkg.Equal(t, "common", cfg.Microsoft.EffectiveTenantID())
}

func TestDatabasePath(t *testing.T) {
	t.Run("plain filesystem path passes through", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DataDir = "/tmp/data"
		got, err := cfg.DatabasePath()
		requirepkg.NoError(t, err, "DatabasePath")
		want := filepath.Join("/tmp/data", "msgvault.db")
		assertpkg.Equal(t, want, got)
	})

	t.Run("file: URI is stripped", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:/var/lib/msgvault.db"
		got, err := cfg.DatabasePath()
		requirepkg.NoError(t, err, "DatabasePath")
		assertpkg.Equal(t, "/var/lib/msgvault.db", got)
	})

	t.Run("file: URI with query string drops query", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:/var/lib/msgvault.db?_journal_mode=WAL&_busy_timeout=5000"
		got, err := cfg.DatabasePath()
		requirepkg.NoError(t, err, "DatabasePath")
		assertpkg.Equal(t, "/var/lib/msgvault.db", got)
	})

	t.Run("file: URI decodes percent-encoded path", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:/var/lib/my%20vault.db"
		got, err := cfg.DatabasePath()
		requirepkg.NoError(t, err, "DatabasePath")
		assertpkg.Equal(t, "/var/lib/my vault.db", got)
	})

	t.Run("file: URI relative path (Opaque)", func(t *testing.T) {
		// SQLite accepts file:rel/path; url.Parse routes that into u.Opaque.
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:msgvault.db"
		got, err := cfg.DatabasePath()
		requirepkg.NoError(t, err, "DatabasePath")
		assertpkg.Equal(t, "msgvault.db", got)
	})

	t.Run("file: URI relative path with percent-encoding (Opaque)", func(t *testing.T) {
		// url.Parse decodes percent-encoding for u.Path but not u.Opaque,
		// so DatabasePath has to PathUnescape the relative-form bytes
		// itself. Without that, "file:my%20vault.db" never matches the
		// on-disk filename "my vault.db" and backups break.
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:my%20vault.db"
		got, err := cfg.DatabasePath()
		requirepkg.NoError(t, err, "DatabasePath")
		assertpkg.Equal(t, "my vault.db", got)
	})

	t.Run("postgres:// is rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "postgres://user@host:5432/db"
		_, err := cfg.DatabasePath()
		requirepkg.Error(t, err, "DatabasePath: expected error for non-file DSN")
	})

	t.Run("empty file: URI is rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:"
		_, err := cfg.DatabasePath()
		requirepkg.Error(t, err, "DatabasePath: expected error for empty file: URI")
	})
}
