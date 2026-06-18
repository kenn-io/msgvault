package oauth

import (
	"log/slog"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"golang.org/x/oauth2/google"
)

func TestHasEmbeddedCredentials(t *testing.T) {
	// Save and restore package vars around the test
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()

	tests := []struct {
		name   string
		id     string
		secret string
		want   bool
	}{
		{"both set", "id", "secret", true},
		{"id only", "id", "", false},
		{"secret only", "", "secret", false},
		{"neither", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oauthClientID = tc.id
			oauthClientSecret = tc.secret
			assertpkg.Equal(t, tc.want, HasEmbeddedCredentials(), "HasEmbeddedCredentials()")
		})
	}
}

func TestEmbeddedConfig(t *testing.T) {
	assert := assertpkg.New(t)
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = "test-client-id"
	oauthClientSecret = "test-client-secret"

	scopes := []string{"scope-a", "scope-b"}
	cfg := EmbeddedConfig(scopes)

	assert.Equal("test-client-id", cfg.ClientID, "ClientID")
	assert.Equal("test-client-secret", cfg.ClientSecret, "ClientSecret")
	assert.Equal(scopes, cfg.Scopes, "Scopes")
	assert.Equal(google.Endpoint, cfg.Endpoint, "Endpoint")
}

func TestNewEmbeddedManager(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = "test-client-id"
	oauthClientSecret = "test-client-secret"

	tokensDir := t.TempDir()
	mgr, err := NewEmbeddedManager(tokensDir, slog.Default(), ScopesEmbedded)
	require.NoError(err, "NewEmbeddedManager")
	require.NotNil(mgr, "NewEmbeddedManager")
	assert.Equal(tokensDir, mgr.tokensDir, "tokensDir")
	assert.True(mgr.isEmbedded, "isEmbedded")
}

func TestNewEmbeddedManagerWithoutCredentials(t *testing.T) {
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = ""
	oauthClientSecret = ""

	_, err := NewEmbeddedManager(t.TempDir(), slog.Default(), ScopesEmbedded)
	requirepkg.Error(t, err, "NewEmbeddedManager should fail when credentials are empty")
}
