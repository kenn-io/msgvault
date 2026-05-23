package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
)

// writeStubClientSecrets writes a minimal valid client_secret.json that
// parseClientSecrets will accept. We only need this to verify the BYO
// path returns a non-nil manager — we don't run any OAuth flow.
func writeStubClientSecrets(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	const stub = `{"installed":{"client_id":"abc","client_secret":"xyz","redirect_uris":["http://localhost"]}}`
	if err := os.WriteFile(path, []byte(stub), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// newTestConfig returns a Config with Data.DataDir set to a fresh temp
// directory. TokensDir() returns <tmp>/tokens, which is what the
// resolver passes to the OAuth manager constructors.
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	}
}

func TestResolveOAuthManager_NamedBYO(t *testing.T) {
	cfg := newTestConfig(t)
	secrets := writeStubClientSecrets(t, cfg.Data.DataDir, "acme.json")
	cfg.OAuth.Apps = map[string]config.OAuthApp{"acme": {ClientSecrets: secrets}}
	mgr, err := resolveOAuthManager(cfg, "acme", oauth.Scopes, slog.Default())
	if err != nil {
		t.Fatalf("resolveOAuthManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("manager is nil")
	}
}

func TestResolveOAuthManager_NamedNotConfigured(t *testing.T) {
	cfg := newTestConfig(t)
	_, err := resolveOAuthManager(cfg, "nonexistent", oauth.Scopes, slog.Default())
	if err == nil {
		t.Fatal("expected error for unknown app name")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error %q should mention the app name", err.Error())
	}
}

func TestResolveOAuthManager_GlobalBYO(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.OAuth.ClientSecrets = writeStubClientSecrets(t, cfg.Data.DataDir, "default.json")
	mgr, err := resolveOAuthManager(cfg, "", oauth.Scopes, slog.Default())
	if err != nil {
		t.Fatalf("resolveOAuthManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("manager is nil")
	}
}

func TestResolveOAuthManager_Embedded(t *testing.T) {
	// Embedded credentials must be non-empty in this test (they are by
	// default — the source has the dev placeholder strings).
	cfg := newTestConfig(t)
	mgr, err := resolveOAuthManager(cfg, "", oauth.Scopes, slog.Default())
	if err != nil {
		t.Fatalf("resolveOAuthManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("manager is nil")
	}
}
