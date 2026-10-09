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
	credentials := config.OAuthApp{ClientSecretsCommand: testutil.SecretCommand(t, "client-once")}
	shared := oauth.NewTokenStore(dir, commands)
	require.NoError(shared.Write(t.Context(), "reader@example.com", []byte(`{"access_token":"shared","expiry":"2099-01-01T00:00:00Z","client_id":"example-client","scopes":["https://www.googleapis.com/auth/gmail.readonly"]}`)))
	mgr, err := NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, dir, commands, "work", "reader@example.com", nil)
	require.NoError(err)
	source, err := mgr.TokenSource(t.Context(), "reader@example.com")
	require.NoError(err)
	token, err := source.Token()
	require.NoError(err)
	assert.Equal("shared", token.AccessToken)
	require.NoError(os.Remove(filepath.Join(os.Getenv("MSGVAULT_TEST_SECRET_ROOT"), "client.read")))
	dedicated := oauth.NewTokenStore(googleTokensDir(dir, "work"), commands)
	require.NoError(dedicated.Write(t.Context(), "reader@example.com", []byte(`{"access_token":"dedicated","expiry":"2099-01-01T00:00:00Z","client_id":"example-client"}`)))
	mgr, err = NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, dir, commands, "work", "reader@example.com", nil)
	require.NoError(err)
	source, err = mgr.TokenSource(t.Context(), "reader@example.com")
	require.NoError(err)
	token, err = source.Token()
	require.NoError(err)
	assert.Equal("dedicated", token.AccessToken)
	require.NoError(os.Remove(filepath.Join(os.Getenv("MSGVAULT_TEST_SECRET_ROOT"), "client.read")))
	commands.ReadCommand = testutil.SecretCommand(t, "fail")
	_, err = NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, dir, commands, "work", "reader@example.com", nil)
	assert.Error(err)
}
