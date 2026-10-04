package carddav

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGoogleCommandTokenNamespacesAndFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commands := config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	dir := t.TempDir()
	credentials := config.OAuthApp{ClientSecretsCommand: testutil.SecretCommand(t, "client")}
	shared := oauth.NewTokenStore(dir, commands)
	require.NoError(shared.Write(t.Context(), "reader@example.com", []byte(`{"access_token":"shared","client_id":"example-client","scopes":["https://www.googleapis.com/auth/gmail.readonly"]}`)))
	mgr, err := NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, dir, commands, "work", "reader@example.com", nil)
	require.NoError(err)
	state, err := mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.Equal("shared", state.Token.AccessToken)
	dedicated := oauth.NewTokenStore(googleTokensDir(dir, "work"), commands)
	require.NoError(dedicated.Write(t.Context(), "reader@example.com", []byte(`{"access_token":"dedicated","client_id":"example-client"}`)))
	mgr, err = NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, dir, commands, "work", "reader@example.com", nil)
	require.NoError(err)
	state, err = mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.Equal("dedicated", state.Token.AccessToken)
	commands.ReadCommand = testutil.SecretCommand(t, "fail")
	_, err = NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, dir, commands, "work", "reader@example.com", nil)
	assert.Error(err)
}

func TestGoogleOAuthManagerAllowsRepairingMalformedFileToken(t *testing.T) {
	required := require.New(t)
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "client.json")
	required.NoError(os.WriteFile(secretsPath, []byte(`{"web":{"client_id":"synthetic-client","client_secret":"synthetic-secret","auth_uri":"https://accounts.example/authorize","token_uri":"https://accounts.example/token","redirect_uris":["https://archive.example/"]}}`), 0600))
	credentials := config.OAuthApp{ClientSecrets: secretsPath}
	tokensDir := filepath.Join(dir, "tokens")
	const email = "person@example.com"
	mgr, err := NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, tokensDir, config.OAuthTokenCommands{}, "", email, nil)
	required.NoError(err)
	tokenPath := mgr.TokenPath(email)
	required.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0700))
	required.NoError(os.WriteFile(tokenPath, []byte("{"), 0600))

	mgr, err = NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, tokensDir, config.OAuthTokenCommands{}, "", email, nil)
	required.NoError(err)
	_, err = mgr.BeginWebAuthorization(email, "https://archive.example/")
	required.NoError(err)
}
