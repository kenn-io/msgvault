package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	requirepkg.NoError(t, os.WriteFile(path, []byte(stub), 0600), "write %s", path)
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
	requirepkg.NoError(t, err, "resolveOAuthManager")
	requirepkg.NotNil(t, mgr, "manager")
}

func TestResolveOAuthManager_NamedNotConfigured(t *testing.T) {
	cfg := newTestConfig(t)
	_, err := resolveOAuthManager(cfg, "nonexistent", oauth.Scopes, slog.Default())
	requirepkg.Error(t, err, "expected error for unknown app name")
	assertpkg.ErrorContains(t, err, "nonexistent")
}

func TestResolveOAuthManager_GlobalBYO(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.OAuth.ClientSecrets = writeStubClientSecrets(t, cfg.Data.DataDir, "default.json")
	mgr, err := resolveOAuthManager(cfg, "", oauth.Scopes, slog.Default())
	requirepkg.NoError(t, err, "resolveOAuthManager")
	requirepkg.NotNil(t, mgr, "manager")
}

func TestResolveOAuthManager_Embedded(t *testing.T) {
	// Embedded credentials must be non-empty in this test (they are by
	// default — the source has the dev placeholder strings).
	cfg := newTestConfig(t)
	mgr, err := resolveOAuthManager(cfg, "", oauth.Scopes, slog.Default())
	requirepkg.NoError(t, err, "resolveOAuthManager")
	requirepkg.NotNil(t, mgr, "manager")
}
