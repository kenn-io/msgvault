package api

import (
	"context"
	"fmt"
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
	tokenStore := oauth.NewTokenStore(cfg.TokensDir(), commands)
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

// Resolving a Google Contacts grant runs credential commands, so requests reuse
// the access token until it expires; a schedule reconcile reads the grant again.
func TestGoogleCardDAVReusesAccessTokenUntilReconcile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, st := savedGoogleCardDAVFixture(t)
	commands := config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	token := fmt.Sprintf(`{"access_token":"cached-access","expiry":"2099-01-01T00:00:00Z","client_id":"synthetic-client","scopes":[%q]}`, oauth.ScopeCardDAV)
	require.NoError(oauth.NewTokenStore(cfg.TokensDir(), commands).Write(t.Context(), cfg.CardDAV.Username, []byte(token)))
	cfg.OAuth.Tokens = commands
	controller, err := NewCardDAVController(cfg, st, testLogger())
	require.NoError(err)
	var scheduled CardDAVOperations
	controller.SetScheduleReconciler(func(_ config.CardDAVConfig, service CardDAVOperations) error {
		scheduled = service
		return nil
	})
	// Each record can be read once; a second resolution fails.
	cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "read-once")
	credential := carddav.Credential{Username: cfg.CardDAV.Username}
	for range 3 {
		access, err := controller.googleBearerToken(t.Context(), credential)
		require.NoError(err)
		assert.Equal("cached-access", access)
	}
	status, err := controller.Status(t.Context(), "")
	require.NoError(err)
	assert.Empty(status.RepairReason)

	require.NoError(controller.ReconcileSchedule(t.Context()))
	assert.NotNil(scheduled, "an unreadable store keeps the schedule for a later retry")
	_, err = controller.googleBearerToken(t.Context(), credential)
	require.Error(err, "reconcile must drop the cached token")
	assert.ErrorIs(err, carddav.ErrGoogleTokenUnavailable)
}

// Reconcile holds the save lock, so a hung credential command must stop when
// the daemon or request context ends.
func TestGoogleCardDAVReconcileHonorsCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg, st := savedGoogleCardDAVFixture(t)
	cfg.OAuth.Tokens = config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	controller, err := NewCardDAVController(cfg, st, testLogger())
	require.NoError(err)
	var scheduled CardDAVOperations
	controller.SetScheduleReconciler(func(_ config.CardDAVConfig, service CardDAVOperations) error {
		scheduled = service
		return nil
	})
	cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "wait")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.ReconcileSchedule(ctx) }()
	const credentialCommandBudget = 15 * time.Second
	require.Eventually(func() bool {
		_, err := os.Stat(filepath.Join(os.Getenv("MSGVAULT_TEST_SECRET_ROOT"), "started"))
		return err == nil
	}, credentialCommandBudget, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
		assert.NotNil(scheduled, "cancellation is not a missing grant")
	case <-time.After(credentialCommandBudget):
		require.Fail("reconcile kept waiting for the credential command after cancellation")
	}
}
