package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServerConfigDefaults(t *testing.T) {
	// Create a temp dir without a config file
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Check server defaults
	if cfg.Server.APIPort != 8080 {
		t.Errorf("Server.APIPort = %d, want 8080", cfg.Server.APIPort)
	}
	if cfg.Server.APIKey != "" {
		t.Errorf("Server.APIKey = %q, want empty", cfg.Server.APIKey)
	}
}

func TestAccountScheduleEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Accounts) != 0 {
		t.Errorf("Accounts = %v, want empty slice", cfg.Accounts)
	}

	scheduled := cfg.ScheduledAccounts()
	if len(scheduled) != 0 {
		t.Errorf("ScheduledAccounts() = %v, want empty slice", scheduled)
	}
}

func TestLoadWithServerConfig(t *testing.T) {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Check server config
	if cfg.Server.APIPort != 9090 {
		t.Errorf("Server.APIPort = %d, want 9090", cfg.Server.APIPort)
	}
	if cfg.Server.APIKey != "test-secret-key" {
		t.Errorf("Server.APIKey = %q, want test-secret-key", cfg.Server.APIKey)
	}

	// Check accounts
	if len(cfg.Accounts) != 2 {
		t.Fatalf("len(Accounts) = %d, want 2", len(cfg.Accounts))
	}

	if cfg.Accounts[0].Email != "test@gmail.com" {
		t.Errorf("Accounts[0].Email = %q, want test@gmail.com", cfg.Accounts[0].Email)
	}
	if cfg.Accounts[0].Schedule != "0 2 * * *" {
		t.Errorf("Accounts[0].Schedule = %q, want '0 2 * * *'", cfg.Accounts[0].Schedule)
	}
	if cfg.Accounts[0].Enabled != true {
		t.Errorf("Accounts[0].Enabled = %v, want true", cfg.Accounts[0].Enabled)
	}
}

func TestScheduledAccounts(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountSchedule{
			{Email: "enabled@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "disabled@gmail.com", Schedule: "0 3 * * *", Enabled: false},
			{Email: "noschedule@gmail.com", Schedule: "", Enabled: true},
			{Email: "both@gmail.com", Schedule: "0 4 * * *", Enabled: true},
		},
	}

	scheduled := cfg.ScheduledAccounts()

	if len(scheduled) != 2 {
		t.Fatalf("len(ScheduledAccounts()) = %d, want 2", len(scheduled))
	}

	// Should contain only enabled accounts with schedules
	emails := make(map[string]bool)
	for _, acc := range scheduled {
		emails[acc.Email] = true
	}

	if !emails["enabled@gmail.com"] {
		t.Error("ScheduledAccounts() missing enabled@gmail.com")
	}
	if !emails["both@gmail.com"] {
		t.Error("ScheduledAccounts() missing both@gmail.com")
	}
	if emails["disabled@gmail.com"] {
		t.Error("ScheduledAccounts() should not include disabled account")
	}
	if emails["noschedule@gmail.com"] {
		t.Error("ScheduledAccounts() should not include account without schedule")
	}
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
				if acc != nil {
					t.Errorf("GetAccountSchedule(%q) = %v, want nil", tt.email, acc)
				}
				return
			}
			if acc == nil {
				t.Fatalf("GetAccountSchedule(%q) = nil, want non-nil", tt.email)
			}
			if acc.Schedule != tt.wantSched {
				t.Errorf("GetAccountSchedule(%q).Schedule = %q, want %q", tt.email, acc.Schedule, tt.wantSched)
			}
		})
	}
}

func TestGetAccountScheduleReturnsCopy(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountSchedule{
			{Email: "test@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}

	// Get a reference and mutate it
	acc := cfg.GetAccountSchedule("test@gmail.com")
	if acc == nil {
		t.Fatal("GetAccountSchedule returned nil")
	}

	// Mutate the returned copy
	acc.Schedule = "modified"
	acc.Enabled = false
	acc.Email = "hacked@gmail.com"

	// Original config must be unchanged
	if cfg.Accounts[0].Schedule != "0 2 * * *" {
		t.Errorf("original Schedule = %q, want '0 2 * * *' (mutation leaked)", cfg.Accounts[0].Schedule)
	}
	if cfg.Accounts[0].Enabled != true {
		t.Error("original Enabled = false, want true (mutation leaked)")
	}
	if cfg.Accounts[0].Email != "test@gmail.com" {
		t.Errorf("original Email = %q, want test@gmail.com (mutation leaked)", cfg.Accounts[0].Email)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get user home dir: %v", err)
	}

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
			got := expandPath(tt.input)
			if got != tt.expected {
				t.Errorf("expandPath(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestLoadEmptyPath(t *testing.T) {
	// Use a temp directory as MSGVAULT_HOME
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	// Load with empty path should use defaults
	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load(\"\") failed: %v", err)
	}

	// Verify default values
	if cfg.HomeDir != tmpDir {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, tmpDir)
	}
	if cfg.Data.DataDir != tmpDir {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, tmpDir)
	}
	if cfg.Sync.RateLimitQPS != 5 {
		t.Errorf("Sync.RateLimitQPS = %d, want 5", cfg.Sync.RateLimitQPS)
	}

	// DatabaseDSN should return default path
	expectedDB := filepath.Join(tmpDir, "msgvault.db")
	if cfg.DatabaseDSN() != expectedDB {
		t.Errorf("DatabaseDSN() = %q, want %q", cfg.DatabaseDSN(), expectedDB)
	}
}

func TestLoadWithConfigFile(t *testing.T) {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load(\"\") failed: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get user home dir: %v", err)
	}

	// Verify paths were expanded
	expectedDataDir := filepath.Join(home, "custom/data")
	if cfg.Data.DataDir != expectedDataDir {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, expectedDataDir)
	}

	expectedSecrets := filepath.Join(home, "secrets/client.json")
	if cfg.OAuth.ClientSecrets != expectedSecrets {
		t.Errorf("OAuth.ClientSecrets = %q, want %q", cfg.OAuth.ClientSecrets, expectedSecrets)
	}

	if cfg.Sync.RateLimitQPS != 10 {
		t.Errorf("Sync.RateLimitQPS = %d, want 10", cfg.Sync.RateLimitQPS)
	}
}

func TestLoadExplicitPathNotFound(t *testing.T) {
	// When --config explicitly specifies a file that doesn't exist, Load should error
	_, err := Load("/nonexistent/path/config.toml", "")
	if err == nil {
		t.Fatal("Load with explicit nonexistent path should return error")
	}
	if got := err.Error(); !strings.Contains(got, "config file not found") {
		t.Errorf("error = %q, want it to contain %q", got, "config file not found")
	}
}

func TestLoadExplicitPathDerivedHomeDir(t *testing.T) {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", configPath, err)
	}

	if cfg.HomeDir != tmpDir {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, tmpDir)
	}
	if cfg.Data.DataDir != tmpDir {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, tmpDir)
	}
	if cfg.Sync.RateLimitQPS != 3 {
		t.Errorf("Sync.RateLimitQPS = %d, want 3", cfg.Sync.RateLimitQPS)
	}

	// Derived paths should use the custom directory
	expectedDB := filepath.Join(tmpDir, "msgvault.db")
	if cfg.DatabaseDSN() != expectedDB {
		t.Errorf("DatabaseDSN() = %q, want %q", cfg.DatabaseDSN(), expectedDB)
	}
	expectedTokens := filepath.Join(tmpDir, "tokens")
	if cfg.TokensDir() != expectedTokens {
		t.Errorf("TokensDir() = %q, want %q", cfg.TokensDir(), expectedTokens)
	}
}

func TestLoadExplicitPathWithDataDirOverride(t *testing.T) {
	// When config file explicitly sets data_dir, that should take precedence
	tmpDir := t.TempDir()
	customDataDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	// Use forward slashes in TOML (works cross-platform)
	configContent := `
[data]
data_dir = "` + filepath.ToSlash(customDataDir) + `"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", configPath, err)
	}

	// HomeDir should be config file's directory
	if cfg.HomeDir != tmpDir {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, tmpDir)
	}
	// DataDir should be the explicit override from config.
	// Normalize both sides since TOML preserves forward slashes on Windows.
	if filepath.Clean(cfg.Data.DataDir) != filepath.Clean(customDataDir) {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, customDataDir)
	}
}

func TestLoadExplicitPathRelativePaths(t *testing.T) {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", configPath, err)
	}

	expectedDataDir := filepath.Join(tmpDir, "data")
	if cfg.Data.DataDir != expectedDataDir {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, expectedDataDir)
	}

	expectedSecrets := filepath.Join(tmpDir, "secrets/client.json")
	if cfg.OAuth.ClientSecrets != expectedSecrets {
		t.Errorf("OAuth.ClientSecrets = %q, want %q", cfg.OAuth.ClientSecrets, expectedSecrets)
	}
}

func TestLoadExplicitPathWithTilde(t *testing.T) {
	// Explicit --config with ~ should be expanded before stat
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get user home dir: %v", err)
	}

	// Create a config file in a temp subdir of home to test ~ expansion
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[sync]\nrate_limit_qps = 7\n"), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	// Construct a ~ path: replace the home prefix with ~
	if !strings.HasPrefix(tmpDir, home) {
		t.Skip("temp dir is not under home directory, cannot test ~ expansion")
	}
	tildePath := "~" + tmpDir[len(home):] + "/config.toml"

	cfg, err := Load(tildePath, "")
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", tildePath, err)
	}

	if cfg.Sync.RateLimitQPS != 7 {
		t.Errorf("Sync.RateLimitQPS = %d, want 7", cfg.Sync.RateLimitQPS)
	}
}

func TestLoadConfigFilePath(t *testing.T) {
	// ConfigFilePath should return the actual loaded path, not the default
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", configPath, err)
	}

	if cfg.ConfigFilePath() != configPath {
		t.Errorf("ConfigFilePath() = %q, want %q", cfg.ConfigFilePath(), configPath)
	}
}

func TestDefaultHomeExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get user home dir: %v", err)
	}

	t.Setenv("MSGVAULT_HOME", "~/.msgvault")
	got := DefaultHome()
	expected := filepath.Join(home, ".msgvault")
	if got != expected {
		t.Errorf("DefaultHome() = %q, want %q", got, expected)
	}
}

// assertTempDirSecured checks that a temp dir has permissions no more
// permissive than 0700. This is umask-tolerant (stricter is fine).
func assertTempDirSecured(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows uses DACLs, not Unix permission bits
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat temp dir: %v", err)
	}
	got := info.Mode().Perm()
	if got&^os.FileMode(0700) != 0 {
		t.Errorf("temp dir perm = %04o, has bits beyond 0700 (extra: %04o)", got, got&^0700)
	}
}

func TestMkTempDir(t *testing.T) {
	t.Run("uses system temp when no preferred dirs", func(t *testing.T) {
		dir, err := MkTempDir("test-*")
		if err != nil {
			t.Fatalf("MkTempDir failed: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		if _, err := os.Stat(dir); err != nil {
			t.Errorf("temp dir does not exist: %v", err)
		}
		assertTempDirSecured(t, dir)
	})

	t.Run("uses preferred dir when available", func(t *testing.T) {
		preferred := t.TempDir()
		dir, err := MkTempDir("test-*", preferred)
		if err != nil {
			t.Fatalf("MkTempDir failed: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		if !strings.HasPrefix(dir, preferred) {
			t.Errorf("temp dir %q not under preferred %q", dir, preferred)
		}
		assertTempDirSecured(t, dir)
	})

	t.Run("skips empty preferred dir strings", func(t *testing.T) {
		dir, err := MkTempDir("test-*", "")
		if err != nil {
			t.Fatalf("MkTempDir failed: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		// Should have used system temp, not errored
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("temp dir does not exist: %v", err)
		}
	})

	t.Run("falls back to system temp when preferred dir is inaccessible", func(t *testing.T) {
		dir, err := MkTempDir("test-*", "/nonexistent-dir-that-does-not-exist")
		if err != nil {
			t.Fatalf("MkTempDir failed: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		// Should have fallen back to system temp
		if strings.Contains(dir, "nonexistent") {
			t.Errorf("should not have used nonexistent dir, got %q", dir)
		}
	})

	t.Run("falls back to msgvault home when system temp is unavailable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("cannot make system temp dir unwritable on Windows")
		}

		// Create a restricted temp dir so os.MkdirTemp("", ...) fails
		restrictedTmp := t.TempDir()
		if err := os.Chmod(restrictedTmp, 0o500); err != nil {
			t.Fatalf("chmod failed: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(restrictedTmp, 0o700) })

		// Probe whether the restriction actually works (root and some ACL
		// configurations can still write to 0500 directories).
		probe, probeErr := os.MkdirTemp(restrictedTmp, "probe-*")
		if probeErr == nil {
			_ = os.Remove(probe)
			t.Skip("chmod 0500 did not restrict writes (running as root or permissive ACLs)")
		}

		// Point TMPDIR to the restricted dir and MSGVAULT_HOME to a writable dir
		msgvaultHome := t.TempDir()
		t.Setenv("TMPDIR", restrictedTmp)
		t.Setenv("MSGVAULT_HOME", msgvaultHome)

		dir, err := MkTempDir("test-*")
		if err != nil {
			t.Fatalf("MkTempDir failed: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		expectedBase := filepath.Join(msgvaultHome, "tmp")
		if !strings.HasPrefix(dir, expectedBase) {
			t.Errorf("temp dir %q not under fallback %q", dir, expectedBase)
		}

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
			tmpDir := t.TempDir()
			t.Setenv("MSGVAULT_HOME", tmpDir)

			configPath := filepath.Join(tmpDir, "config.toml")
			if err := os.WriteFile(configPath, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("failed to write config file: %v", err)
			}

			_, err := Load("", "")
			if err == nil {
				t.Fatal("Load should fail on TOML backslash error")
			}

			errMsg := err.Error()
			if !strings.Contains(errMsg, "hint:") {
				t.Errorf("error should contain hint, got: %s", errMsg)
			}
			if !strings.Contains(errMsg, "forward slashes") {
				t.Errorf("error should mention forward slashes, got: %s", errMsg)
			}
			if !strings.Contains(errMsg, "single quotes") {
				t.Errorf("error should mention single quotes, got: %s", errMsg)
			}
		})
	}
}

func TestLoadWithHomeDir(t *testing.T) {
	homeDir := t.TempDir()

	cfg, err := Load("", homeDir)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.HomeDir != homeDir {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, homeDir)
	}
	if cfg.Data.DataDir != homeDir {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, homeDir)
	}

	// Derived paths should use the home directory
	expectedDB := filepath.Join(homeDir, "msgvault.db")
	if cfg.DatabaseDSN() != expectedDB {
		t.Errorf("DatabaseDSN() = %q, want %q", cfg.DatabaseDSN(), expectedDB)
	}
	expectedTokens := filepath.Join(homeDir, "tokens")
	if cfg.TokensDir() != expectedTokens {
		t.Errorf("TokensDir() = %q, want %q", cfg.TokensDir(), expectedTokens)
	}
}

func TestLoadWithHomeDirReadsConfig(t *testing.T) {
	// --home should load config.toml from that directory
	homeDir := t.TempDir()
	configPath := filepath.Join(homeDir, "config.toml")
	configContent := `[sync]
rate_limit_qps = 42
`
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := Load("", homeDir)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Sync.RateLimitQPS != 42 {
		t.Errorf("Sync.RateLimitQPS = %d, want 42", cfg.Sync.RateLimitQPS)
	}
	if cfg.HomeDir != homeDir {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, homeDir)
	}
}

func TestLoadWithHomeDirExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get user home dir: %v", err)
	}

	cfg, err := Load("", "~/custom-data")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	expected := filepath.Join(home, "custom-data")
	if cfg.HomeDir != expected {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, expected)
	}
	if cfg.Data.DataDir != expected {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, expected)
	}
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
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() should succeed with deprecated mcp_enabled, got error: %v", err)
	}

	if cfg.Server.APIPort != 9090 {
		t.Errorf("Server.APIPort = %d, want 9090", cfg.Server.APIPort)
	}
}

func TestNewDefaultConfig(t *testing.T) {
	// Use a temp directory as MSGVAULT_HOME
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	cfg := NewDefaultConfig()

	if cfg.HomeDir != tmpDir {
		t.Errorf("HomeDir = %q, want %q", cfg.HomeDir, tmpDir)
	}
	if cfg.Data.DataDir != tmpDir {
		t.Errorf("Data.DataDir = %q, want %q", cfg.Data.DataDir, tmpDir)
	}
	if cfg.Sync.RateLimitQPS != 5 {
		t.Errorf("Sync.RateLimitQPS = %d, want 5", cfg.Sync.RateLimitQPS)
	}
}

func TestSaveAndLoad_RoundTrip(t *testing.T) {
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

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Load it back
	loaded, err := Load(cfg.ConfigFilePath(), "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify all fields survived the round trip
	if loaded.OAuth.ClientSecrets != cfg.OAuth.ClientSecrets {
		t.Errorf("OAuth.ClientSecrets = %q, want %q",
			loaded.OAuth.ClientSecrets, cfg.OAuth.ClientSecrets)
	}
	if loaded.Sync.RateLimitQPS != 10 {
		t.Errorf("Sync.RateLimitQPS = %d, want 10", loaded.Sync.RateLimitQPS)
	}
	if loaded.Server.APIPort != 9090 {
		t.Errorf("Server.APIPort = %d, want 9090", loaded.Server.APIPort)
	}
	if loaded.Server.APIKey != "my-server-key" {
		t.Errorf("Server.APIKey = %q, want my-server-key", loaded.Server.APIKey)
	}
	if loaded.Remote.URL != "http://nas:8080" {
		t.Errorf("Remote.URL = %q, want http://nas:8080", loaded.Remote.URL)
	}
	if loaded.Remote.APIKey != "my-remote-key" {
		t.Errorf("Remote.APIKey = %q, want my-remote-key", loaded.Remote.APIKey)
	}
	if !loaded.Remote.AllowInsecure {
		t.Error("Remote.AllowInsecure = false, want true")
	}
	if len(loaded.Accounts) != 1 {
		t.Fatalf("len(Accounts) = %d, want 1", len(loaded.Accounts))
	}
	if loaded.Accounts[0].Email != "user@gmail.com" {
		t.Errorf("Accounts[0].Email = %q, want user@gmail.com",
			loaded.Accounts[0].Email)
	}
}

func TestSave_CreatesFileWithSecurePermissions(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(cfg.ConfigFilePath())
	if err != nil {
		t.Fatalf("Stat config: %v", err)
	}

	// Should have no group/other permissions (0600 or stricter)
	// Windows doesn't support Unix file permissions.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Errorf("config perm = %04o, want no group/other access",
			info.Mode().Perm())
	}
}

func TestSave_TightensWeakPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions not supported on Windows")
	}

	tmpDir := t.TempDir()
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir

	// Pre-create config file with overly permissive mode
	path := cfg.ConfigFilePath()
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Errorf("Save should tighten perms: got %04o, want 0600",
			info.Mode().Perm())
	}
}

func TestSave_FollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}

	t.Run("absolute target", func(t *testing.T) {
		tmpDir := t.TempDir()
		targetDir := t.TempDir()
		targetPath := filepath.Join(targetDir, "actual-config.toml")
		linkPath := filepath.Join(tmpDir, "config.toml")

		if err := os.Symlink(targetPath, linkPath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		cfg := NewDefaultConfig()
		cfg.HomeDir = tmpDir
		cfg.Sync.RateLimitQPS = 77

		if err := cfg.Save(); err != nil {
			t.Fatalf("Save() error = %v", err)
		}

		linkTarget, err := os.Readlink(linkPath)
		if err != nil {
			t.Fatalf("symlink was replaced: %v", err)
		}
		if linkTarget != targetPath {
			t.Errorf("symlink target = %q, want %q", linkTarget, targetPath)
		}

		loaded, err := Load(targetPath, "")
		if err != nil {
			t.Fatalf("Load target: %v", err)
		}
		if loaded.Sync.RateLimitQPS != 77 {
			t.Errorf("RateLimitQPS = %d, want 77", loaded.Sync.RateLimitQPS)
		}
	})

	t.Run("relative target", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Create subdir for the actual file
		subDir := filepath.Join(tmpDir, "real")
		if err := os.Mkdir(subDir, 0700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		targetPath := filepath.Join(subDir, "config.toml")
		linkPath := filepath.Join(tmpDir, "config.toml")

		// Relative symlink: config.toml → real/config.toml
		if err := os.Symlink("real/config.toml", linkPath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		cfg := NewDefaultConfig()
		cfg.HomeDir = tmpDir
		cfg.Sync.RateLimitQPS = 88

		if err := cfg.Save(); err != nil {
			t.Fatalf("Save() error = %v", err)
		}

		// Symlink must still be intact
		linkTarget, err := os.Readlink(linkPath)
		if err != nil {
			t.Fatalf("symlink was replaced: %v", err)
		}
		if linkTarget != "real/config.toml" {
			t.Errorf("symlink target = %q, want %q",
				linkTarget, "real/config.toml")
		}

		// Target file should contain the saved config
		loaded, err := Load(targetPath, "")
		if err != nil {
			t.Fatalf("Load target: %v", err)
		}
		if loaded.Sync.RateLimitQPS != 88 {
			t.Errorf("RateLimitQPS = %d, want 88",
				loaded.Sync.RateLimitQPS)
		}
	})
}

func TestSave_FailurePreservesExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cannot make directory unwritable on Windows")
	}

	tmpDir := t.TempDir()

	// Save initial valid config
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir
	cfg.Sync.RateLimitQPS = 5
	if err := cfg.Save(); err != nil {
		t.Fatalf("initial Save: %v", err)
	}

	// Read back original content
	originalBytes, err := os.ReadFile(cfg.ConfigFilePath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Make directory unwritable so CreateTemp fails
	if err := os.Chmod(tmpDir, 0500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
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
	if err := cfg.Save(); err == nil {
		t.Fatal("Save should fail when directory is unwritable")
	}

	// Restore permissions to verify state
	if err := os.Chmod(tmpDir, 0700); err != nil {
		t.Fatalf("Chmod restore: %v", err)
	}

	// Original config should be intact
	currentBytes, err := os.ReadFile(cfg.ConfigFilePath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(currentBytes) != string(originalBytes) {
		t.Error("config file was corrupted after failed Save")
	}

	// No temp files should be left behind
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSave_OverwritesExisting(t *testing.T) {
	tmpDir := t.TempDir()

	// Save initial config
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir
	cfg.Sync.RateLimitQPS = 5
	if err := cfg.Save(); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}

	// Update and save again
	cfg.Sync.RateLimitQPS = 42
	if err := cfg.Save(); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}

	// Load and verify the update took effect
	loaded, err := Load(cfg.ConfigFilePath(), "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Sync.RateLimitQPS != 42 {
		t.Errorf("Sync.RateLimitQPS = %d, want 42", loaded.Sync.RateLimitQPS)
	}
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
				if err == nil {
					t.Errorf("ClientSecretsFor(%q) error = nil, want error", tt.appName)
				}
				return
			}
			if err != nil {
				t.Errorf("ClientSecretsFor(%q) error = %v", tt.appName, err)
				return
			}
			if got != tt.want {
				t.Errorf("ClientSecretsFor(%q) = %q, want %q", tt.appName, got, tt.want)
			}
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
			if got := cfg.ServiceAccountKeyFor(tt.appName); got != tt.want {
				t.Errorf("ServiceAccountKeyFor(%q) = %q, want %q", tt.appName, got, tt.want)
			}
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
			got := tt.config.HasAnyConfig()
			if got != tt.want {
				t.Errorf("HasAnyConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadWithNamedOAuthApps(t *testing.T) {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	// Default should be expanded
	expectedDefault := filepath.Join(home, "secrets/default.json")
	if cfg.OAuth.ClientSecrets != expectedDefault {
		t.Errorf("ClientSecrets = %q, want %q", cfg.OAuth.ClientSecrets, expectedDefault)
	}

	// Named apps should be expanded
	expectedAcme := filepath.Join(home, "secrets/acme.json")
	acme, ok := cfg.OAuth.Apps["acme"]
	if !ok {
		t.Fatal("Apps[acme] not found")
	}
	if acme.ClientSecrets != expectedAcme {
		t.Errorf("Apps[acme].ClientSecrets = %q, want %q", acme.ClientSecrets, expectedAcme)
	}

	// Absolute paths should be unchanged
	personal, ok := cfg.OAuth.Apps["personal"]
	if !ok {
		t.Fatal("Apps[personal] not found")
	}
	if personal.ClientSecrets != "/absolute/personal.json" {
		t.Errorf("Apps[personal].ClientSecrets = %q, want /absolute/personal.json", personal.ClientSecrets)
	}

	// HasAnyConfig should be true
	if !cfg.OAuth.HasAnyConfig() {
		t.Error("HasAnyConfig() = false, want true")
	}
}

func TestLoadExpandsVectorDBPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

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
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	expected := filepath.Join(home, "custom/vectors.db")
	if cfg.Vector.DBPath != expected {
		t.Errorf("Vector.DBPath = %q, want %q", cfg.Vector.DBPath, expected)
	}
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
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	expected := filepath.Join(tmpDir, "sub/vectors.db")
	if cfg.Vector.DBPath != expected {
		t.Errorf("Vector.DBPath = %q, want %q", cfg.Vector.DBPath, expected)
	}
}

// TestLoadReappliesVectorDefaults verifies that a zero-valued numeric
// field in the TOML file (e.g. max_retries = 0) gets normalized back to
// the documented default so users cannot accidentally disable retries or
// timeouts. Preprocess booleans use pointer semantics and are exempt
// from this re-defaulting.
func TestLoadReappliesVectorDefaults(t *testing.T) {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Vector.Embeddings.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3 (re-defaulted from explicit 0)", cfg.Vector.Embeddings.MaxRetries)
	}
	// NOTE: TOML "0s" currently decodes to time.Duration(0); post-decode
	// ApplyDefaults lifts it back to 30s to avoid a hang.
	if cfg.Vector.Embeddings.Timeout <= 0 {
		t.Errorf("Timeout = %v, want positive (re-defaulted from explicit 0s)", cfg.Vector.Embeddings.Timeout)
	}
	// Explicit false in the TOML file must survive.
	if cfg.Vector.Preprocess.StripSignaturesEnabled() != false {
		t.Errorf("StripSignaturesEnabled() = %v, want false (user explicitly set)", cfg.Vector.Preprocess.StripSignaturesEnabled())
	}
	// Omitted sibling stays at default true.
	if cfg.Vector.Preprocess.StripQuotesEnabled() != true {
		t.Errorf("StripQuotesEnabled() = %v, want true (unset → default)", cfg.Vector.Preprocess.StripQuotesEnabled())
	}
}

func TestLoadWithNamedOAuthApps_RelativePaths(t *testing.T) {
	tmpDir := t.TempDir()

	configContent := `
[oauth.apps.acme]
client_secrets = "secrets/acme.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	// Use explicit --config so relative paths resolve against config dir
	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	expectedAcme := filepath.Join(tmpDir, "secrets/acme.json")
	acme, ok := cfg.OAuth.Apps["acme"]
	if !ok {
		t.Fatal("Apps[acme] not found")
	}
	if acme.ClientSecrets != expectedAcme {
		t.Errorf("Apps[acme].ClientSecrets = %q, want %q", acme.ClientSecrets, expectedAcme)
	}
}

func TestLoadWithServiceAccountKeysExpandsPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[oauth]
service_account_key = "~/keys/default-service-account.json"

[oauth.apps.workspace]
service_account_key = "~/keys/workspace-service-account.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	expectedDefault := filepath.Join(home, "keys/default-service-account.json")
	if cfg.OAuth.ServiceAccountKey != expectedDefault {
		t.Errorf("ServiceAccountKey = %q, want %q", cfg.OAuth.ServiceAccountKey, expectedDefault)
	}

	expectedWorkspace := filepath.Join(home, "keys/workspace-service-account.json")
	workspace := cfg.OAuth.Apps["workspace"]
	if workspace.ServiceAccountKey != expectedWorkspace {
		t.Errorf("Apps[workspace].ServiceAccountKey = %q, want %q", workspace.ServiceAccountKey, expectedWorkspace)
	}
}

func TestLoadWithServiceAccountKeysResolvesRelativePaths(t *testing.T) {
	tmpDir := t.TempDir()

	configContent := `
[oauth]
service_account_key = "keys/default-service-account.json"

[oauth.apps.workspace]
service_account_key = "keys/workspace-service-account.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	expectedDefault := filepath.Join(tmpDir, "keys/default-service-account.json")
	if cfg.OAuth.ServiceAccountKey != expectedDefault {
		t.Errorf("ServiceAccountKey = %q, want %q", cfg.OAuth.ServiceAccountKey, expectedDefault)
	}

	expectedWorkspace := filepath.Join(tmpDir, "keys/workspace-service-account.json")
	workspace := cfg.OAuth.Apps["workspace"]
	if workspace.ServiceAccountKey != expectedWorkspace {
		t.Errorf("Apps[workspace].ServiceAccountKey = %q, want %q", workspace.ServiceAccountKey, expectedWorkspace)
	}
}

func TestLoadNamedAppsOnly_NoDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)

	configContent := `
[oauth.apps.acme]
client_secrets = "/path/to/acme.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	cfg, err := Load("", "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Default should be empty
	if cfg.OAuth.ClientSecrets != "" {
		t.Errorf("ClientSecrets = %q, want empty", cfg.OAuth.ClientSecrets)
	}

	// HasAnyConfig should still be true
	if !cfg.OAuth.HasAnyConfig() {
		t.Error("HasAnyConfig() = false, want true")
	}

	// ClientSecretsFor("") should fail
	_, err = cfg.OAuth.ClientSecretsFor("")
	if err == nil {
		t.Error("ClientSecretsFor(\"\") should error with no default")
	}

	// ClientSecretsFor("acme") should work
	path, err := cfg.OAuth.ClientSecretsFor("acme")
	if err != nil {
		t.Errorf("ClientSecretsFor(acme) error = %v", err)
	}
	if path != "/path/to/acme.json" {
		t.Errorf("ClientSecretsFor(acme) = %q, want /path/to/acme.json", path)
	}
}

func TestSave_AllowInsecureRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	// Save with AllowInsecure = false (default)
	cfg := NewDefaultConfig()
	cfg.HomeDir = tmpDir
	cfg.Remote.URL = "https://nas:8080"
	cfg.Remote.APIKey = "key"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(cfg.ConfigFilePath(), "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Remote.AllowInsecure {
		t.Error("AllowInsecure should be false when not set")
	}

	// Now save with AllowInsecure = true
	cfg.Remote.AllowInsecure = true
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err = Load(cfg.ConfigFilePath(), "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !loaded.Remote.AllowInsecure {
		t.Error("AllowInsecure should be true after saving with true")
	}
}

func TestMicrosoftConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configContent := `
[microsoft]
client_id = "test-client-id-123"
tenant_id = "my-tenant"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Microsoft.ClientID != "test-client-id-123" {
		t.Errorf("Microsoft.ClientID = %q, want %q", cfg.Microsoft.ClientID, "test-client-id-123")
	}
	if cfg.Microsoft.TenantID != "my-tenant" {
		t.Errorf("Microsoft.TenantID = %q, want %q", cfg.Microsoft.TenantID, "my-tenant")
	}
}

func TestMicrosoftConfig_DefaultTenant(t *testing.T) {
	cfg := NewDefaultConfig()
	if cfg.Microsoft.EffectiveTenantID() != "common" {
		t.Errorf("EffectiveTenantID() = %q, want %q", cfg.Microsoft.EffectiveTenantID(), "common")
	}
}

func TestDatabasePath(t *testing.T) {
	t.Run("plain filesystem path passes through", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DataDir = "/tmp/data"
		got, err := cfg.DatabasePath()
		if err != nil {
			t.Fatalf("DatabasePath: %v", err)
		}
		want := filepath.Join("/tmp/data", "msgvault.db")
		if got != want {
			t.Errorf("DatabasePath() = %q, want %q", got, want)
		}
	})

	t.Run("file: URI is stripped", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:/var/lib/msgvault.db"
		got, err := cfg.DatabasePath()
		if err != nil {
			t.Fatalf("DatabasePath: %v", err)
		}
		if got != "/var/lib/msgvault.db" {
			t.Errorf("DatabasePath() = %q, want '/var/lib/msgvault.db'", got)
		}
	})

	t.Run("file: URI with query string drops query", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:/var/lib/msgvault.db?_journal_mode=WAL&_busy_timeout=5000"
		got, err := cfg.DatabasePath()
		if err != nil {
			t.Fatalf("DatabasePath: %v", err)
		}
		if got != "/var/lib/msgvault.db" {
			t.Errorf("DatabasePath() = %q, want '/var/lib/msgvault.db'", got)
		}
	})

	t.Run("file: URI decodes percent-encoded path", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:/var/lib/my%20vault.db"
		got, err := cfg.DatabasePath()
		if err != nil {
			t.Fatalf("DatabasePath: %v", err)
		}
		if got != "/var/lib/my vault.db" {
			t.Errorf("DatabasePath() = %q, want '/var/lib/my vault.db'", got)
		}
	})

	t.Run("file: URI relative path (Opaque)", func(t *testing.T) {
		// SQLite accepts file:rel/path; url.Parse routes that into u.Opaque.
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:msgvault.db"
		got, err := cfg.DatabasePath()
		if err != nil {
			t.Fatalf("DatabasePath: %v", err)
		}
		if got != "msgvault.db" {
			t.Errorf("DatabasePath() = %q, want 'msgvault.db'", got)
		}
	})

	t.Run("file: URI relative path with percent-encoding (Opaque)", func(t *testing.T) {
		// url.Parse decodes percent-encoding for u.Path but not u.Opaque,
		// so DatabasePath has to PathUnescape the relative-form bytes
		// itself. Without that, "file:my%20vault.db" never matches the
		// on-disk filename "my vault.db" and backups break.
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:my%20vault.db"
		got, err := cfg.DatabasePath()
		if err != nil {
			t.Fatalf("DatabasePath: %v", err)
		}
		if got != "my vault.db" {
			t.Errorf("DatabasePath() = %q, want 'my vault.db'", got)
		}
	})

	t.Run("postgres:// is rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "postgres://user@host:5432/db"
		_, err := cfg.DatabasePath()
		if err == nil {
			t.Fatal("DatabasePath: expected error for non-file DSN, got nil")
		}
	})

	t.Run("empty file: URI is rejected", func(t *testing.T) {
		cfg := &Config{}
		cfg.Data.DatabaseURL = "file:"
		_, err := cfg.DatabasePath()
		if err == nil {
			t.Fatal("DatabasePath: expected error for empty file: URI, got nil")
		}
	})
}
