package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestUploadTokenUsesCommandStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commands := config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	cfg := &config.Config{HomeDir: t.TempDir(), OAuth: config.OAuthConfig{Tokens: commands}, Server: config.ServerConfig{APIPort: 8080}}
	cfg.Data.DataDir = cfg.HomeDir
	server := NewServer(cfg, nil, newMockScheduler(), testLogger())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/reader@example.com", strings.NewReader(`{"refresh_token":"example-refresh","client_id":"example-client","scopes":["example-read"]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusCreated, response.Code, response.Body.String())
	data, err := oauth.NewTokenStore(cfg.TokensDir(), commands).Read(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.Contains(string(data), "example-refresh")
	assert.Contains(string(data), "example-client")
	_, err = os.Stat(filepath.Join(cfg.TokensDir(), "reader@example.com.json"))
	require.ErrorIs(err, os.ErrNotExist)
	commands.WriteCommand = testutil.SecretCommand(t, "fail")
	cfg.OAuth.Tokens = commands
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/reader@example.com", strings.NewReader(`{"refresh_token":"new-example"}`))
	request.Header.Set("Content-Type", "application/json")
	server.Router().ServeHTTP(response, request)
	assert.Equal(http.StatusInternalServerError, response.Code)
	assert.NotContains(response.Body.String(), "example-private")
}

func TestGoogleCommandReadsHonorRequestCancellation(t *testing.T) {
	commands := config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	cfg := &config.Config{HomeDir: t.TempDir(), OAuth: config.OAuthConfig{ClientSecretsCommand: testutil.SecretCommand(t, "client"), Tokens: commands}}
	cfg.Data.DataDir = cfg.HomeDir
	controller := &CardDAVController{cfg: cfg}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := controller.googleBearerToken(ctx, carddav.Credential{Username: "reader@example.com"})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestGoogleAuthorizationFinalReadHonorsCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commands := config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.OAuth.ClientSecretsCommand = testutil.SecretCommand(t, "client")
	cfg.OAuth.Tokens = commands
	credentials, err := cfg.OAuth.CredentialsFor("")
	require.NoError(err)
	mgr, err := carddav.NewGoogleOAuthManagerWithCredentials(t.Context(), credentials, cfg.TokensDir(), commands, "", "reader@example.com", nil)
	require.NoError(err)
	tokenStore := oauth.NewTokenStore(filepath.Dir(mgr.TokenPath("reader@example.com")), commands)
	require.NoError(tokenStore.Write(t.Context(), "reader@example.com", []byte(`{"refresh_token":"example-refresh","client_id":"example-client"}`)))
	cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "read-then-wait")
	controller := &CardDAVController{cfg: cfg}
	srv := &Server{cardDAV: controller, logger: testLogger()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "https://archive.example/api/v1/carddav/google/authorize", strings.NewReader(`{"email":"reader@example.com","redirect_uri":"https://archive.example/"}`)).WithContext(ctx)
	request.Header.Set("Origin", "https://archive.example")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); srv.handleGoogleCardDAVAuthorize(response, request) }()
	const credentialCommandBudget = 15 * time.Second
	require.Eventually(func() bool {
		_, err := os.Stat(filepath.Join(os.Getenv("MSGVAULT_TEST_SECRET_ROOT"), "started"))
		return err == nil
	}, credentialCommandBudget, 10*time.Millisecond)
	cancel()
	require.Eventually(func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, credentialCommandBudget, 10*time.Millisecond)
	assert.Empty(controller.googleAuthorizations)
	assert.Empty(response.Body.String())
}
