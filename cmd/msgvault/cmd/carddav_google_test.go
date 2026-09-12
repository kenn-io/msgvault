package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
)

func TestAuthorizeGoogleCardDAVValidatesEmailAndExplainsMissingSecrets(t *testing.T) {
	dir := t.TempDir()
	withStoreResolverConfig(t, &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}, OAuth: config.OAuthConfig{ClientSecrets: filepath.Join(dir, "missing.json")}})
	for _, tc := range []struct{ email, wantError string }{
		{"person name@example.com", "invalid email address"},
		{"Person <person@example.com>", "invalid email address"},
		{" person@EXAMPLE.com ", "OAuth client secrets file not accessible"},
	} {
		t.Run(tc.email, func(t *testing.T) {
			cmd := newAuthorizeGoogleCardDAVCmd()
			err := cmd.RunE(cmd, []string{tc.email})
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

func TestAuthorizeGoogleCardDAVAllowsClientRotation(t *testing.T) {
	required := require.New(t)
	dir := t.TempDir()
	secrets := filepath.Join(dir, "client.json")
	required.NoError(os.WriteFile(secrets, []byte(`{"installed":{"client_id":"selected-client","client_secret":"synthetic-secret","auth_uri":"https://accounts.example/authorize","token_uri":"https://accounts.example/token","redirect_uris":["http://localhost"]}}`), 0600))
	withStoreResolverConfig(t, &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}, OAuth: config.OAuthConfig{ClientSecrets: secrets}})
	mgr, err := carddav.NewGoogleOAuthManager(secrets, cfg.TokensDir(), "", "person@example.com", nil)
	required.NoError(err)
	required.NoError(os.MkdirAll(filepath.Dir(mgr.TokenPath("person@example.com")), 0700))
	oldToken := []byte(`{"access_token":"synthetic-old-access","client_id":"previous-client","scopes":["https://www.googleapis.com/auth/carddav"]}`)
	required.NoError(os.WriteFile(mgr.TokenPath("person@example.com"), oldToken, 0600))
	for _, manual := range []string{"false", "true"} {
		t.Run("manual="+manual, func(t *testing.T) {
			required := require.New(t)
			cmd := newAuthorizeGoogleCardDAVCmd()
			required.NoError(cmd.Flags().Set("no-browser", manual))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cmd.SetContext(ctx)
			required.ErrorIs(cmd.RunE(cmd, []string{"person@example.com"}), context.Canceled)
			unchanged, err := os.ReadFile(mgr.TokenPath("person@example.com"))
			required.NoError(err)
			assert.Equal(t, oldToken, unchanged)
		})
	}
}
