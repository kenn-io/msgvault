package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func savedGoogleCardDAVFixture(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	secrets := filepath.Join(dir, "client.json")
	require.NoError(t, os.WriteFile(secrets, []byte(`{"web":{"client_id":"synthetic-client","client_secret":"synthetic-secret","redirect_uris":["https://archive.example/"]}}`), 0600))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.OAuth.ClientSecrets = secrets
	cfg.CardDAV = config.CardDAVConfig{Provider: "google", BaseURL: carddav.GoogleDiscoveryURL, Username: "person@example.com", Enabled: true}
	require.NoError(t, cfg.Save())
	st := testutil.NewTestStore(t)
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		PrincipalURL: "https://www.googleapis.com/principal/", HomeURL: "https://www.googleapis.com/contacts/",
	})
	require.NoError(t, err)
	require.NoError(t, carddav.SaveCredential(cfg.TokensDir(), carddav.Credential{
		Google: true, BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username, ConnectionGeneration: 1,
	}))
	return cfg, st
}

func TestGoogleCardDAVRuntimeRecoversAfterCLIAuthorization(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	cfg, st := savedGoogleCardDAVFixture(t)
	controller, err := NewCardDAVController(cfg, st, testLogger())
	required.NoError(err)
	service := controller.Current()
	assertions.NotNil(service, "missing OAuth tokens must not prevent constructing the runtime")
	status, err := controller.Status(t.Context())
	required.NoError(err)
	assertions.Equal("google_authorization_required", status.RepairReason)
	assertions.False(status.CredentialConfigured)
	assertions.False(controller.passwordConfigured(t.Context(), cfg.CardDAV.BaseURL, cfg.CardDAV.Username))

	// A matching mail authorization becomes reusable after daemon startup.
	sharedPath := filepath.Join(cfg.TokensDir(), cfg.CardDAV.Username+".json")
	token := fmt.Sprintf(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","client_id":"synthetic-client","scopes":[%q]}`, oauth.ScopeCardDAV)
	required.NoError(os.WriteFile(sharedPath, []byte(token), 0600))
	status, err = controller.Status(t.Context())
	required.NoError(err)
	assertions.Empty(status.RepairReason)
	assertions.True(status.Available)
	assertions.True(status.CredentialConfigured)
	assertions.Same(service, controller.Current())

	// Mail switches clients, so CLI Contacts authorization now uses its own directory.
	required.NoError(os.WriteFile(sharedPath, []byte(strings.ReplaceAll(token, "synthetic-client", "mail-client")), 0600))
	status, err = controller.Status(t.Context())
	required.NoError(err)
	assertions.Equal("google_authorization_required", status.RepairReason)
	mgr, err := carddav.NewGoogleOAuthManager(cfg.OAuth.ClientSecrets, cfg.TokensDir(), "", cfg.CardDAV.Username, testLogger())
	required.NoError(err)
	required.NoError(os.MkdirAll(filepath.Dir(mgr.TokenPath(cfg.CardDAV.Username)), 0700))
	required.NoError(os.WriteFile(mgr.TokenPath(cfg.CardDAV.Username), []byte(token), 0600))
	status, err = controller.Status(t.Context())
	required.NoError(err)
	assertions.Empty(status.RepairReason)
	assertions.True(status.Available)
	assertions.True(status.CredentialConfigured)
	assertions.Same(service, controller.Current())
}

func TestGoogleCardDAVScheduleSaveDoesNotRequireAuthorization(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	cfg, st := savedGoogleCardDAVFixture(t)
	controller := &CardDAVController{cfg: cfg, store: st, service: &controlledCardDAVCandidate{}}
	response, err := controller.Save(t.Context(), CardDAVAccountRequest{
		Provider: "google", Username: cfg.CardDAV.Username, Enabled: new(true), Schedule: "0 3 * * *",
	})
	required.NoError(err)
	assertions.Equal("0 3 * * *", response.Schedule)
	persisted, err := config.Load(cfg.ConfigFilePath(), cfg.HomeDir)
	required.NoError(err)
	assertions.Equal("0 3 * * *", persisted.CardDAV.Schedule)
}

func TestCardDAVGoogleAccountSelection(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	req := normalizeCardDAVAccountRequest(CardDAVAccountRequest{Provider: "google", Username: "person@example.com", OAuthApp: "contacts", Enabled: new(true)})
	required.NoError(validateCardDAVAccountRequest(req))
	assertions.Equal(carddav.GoogleDiscoveryURL, req.BaseURL)
	controller := &CardDAVController{}
	credential, err := controller.credentialForRequest(t.Context(), req)
	required.NoError(err)
	assertions.True(credential.Google)
	assertions.Empty(credential.Password)
	assertions.Equal("contacts", credential.OAuthApp)
	credential.ConnectionGeneration = 1
	dir := t.TempDir()
	required.NoError(carddav.SaveCredential(dir, credential))
	saved, err := carddav.LoadCredential(dir)
	required.NoError(err)
	assertions.True(cardDAVCredentialMatchesConfig(saved, config.CardDAVConfig{Provider: "google", OAuthApp: "contacts"}))
	assertions.False(cardDAVCredentialMatchesConfig(saved, config.CardDAVConfig{}))
	req.Password = "synthetic-password"
	required.Error(validateCardDAVAccountRequest(req))
}

func TestCardDAVGoogleAuthorizationConsumedOnceAndExpires(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	flow := &oauth.WebAuthorization{State: "synthetic-state"}
	controller := &CardDAVController{googleAuthorizations: map[string]cardDAVGoogleAuthorization{
		flow.State: {flow: flow, expires: time.Now().Add(time.Minute)},
		"expired":  {flow: flow, expires: time.Now().Add(-time.Minute)},
	}}
	got, err := controller.takeGoogleAuthorization(flow.State)
	required.NoError(err)
	assertions.Same(flow, got)
	_, err = controller.takeGoogleAuthorization(flow.State)
	required.Error(err)
	_, err = controller.takeGoogleAuthorization("expired")
	required.Error(err)
}

// Only the external Google exchange and profile lookup are simulated. The
// router, operation gate, authorization state, and token persistence are real.
type googleAuthorizationTransport func(*http.Request) (*http.Response, error)

func (f googleAuthorizationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGoogleAuthorizationCompletesWhileArchiveGateHeld(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "archive operation")
	required.True(ok)
	defer release()
	cfg, st := savedGoogleCardDAVFixture(t)
	cfg.CardDAV.Schedule = "0 3 * * *"
	secrets := cfg.OAuth.ClientSecrets
	required.NoError(os.WriteFile(secrets, []byte(`{"web":{"client_id":"synthetic-client","client_secret":"synthetic-secret","auth_uri":"https://accounts.example/authorize","token_uri":"https://accounts.example/token","redirect_uris":["https://archive.example/"]}}`), 0600))
	mailToken := []byte(`{"access_token":"mail-access","refresh_token":"mail-refresh","client_id":"other-mail-client","scopes":["https://www.googleapis.com/auth/gmail.readonly"]}`)
	mailPath := filepath.Join(cfg.TokensDir(), "person@example.com.json")
	required.NoError(os.WriteFile(mailPath, mailToken, 0600))
	controller, err := NewCardDAVController(cfg, st, testLogger())
	required.NoError(err)
	reconciled := 0
	controller.SetScheduleReconciler(func(settings config.CardDAVConfig, service CardDAVOperations) error {
		reconciled++
		assertions.Equal(cfg.CardDAV, settings)
		assertions.Same(controller.Current(), service, "authorization must make the existing runtime eligible for scheduling")
		return nil
	})
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{}, Logger: testLogger(), OperationGate: gate, CardDAV: controller})
	provider := &http.Client{Transport: googleAuthorizationTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.String() {
		case "https://accounts.example/token":
			body = fmt.Sprintf(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expires_in":3600,"scope":%q}`, oauth.ScopeCardDAV+" "+oauth.ScopeUserinfoEmail)
		case "https://www.googleapis.com/oauth2/v2/userinfo":
			assertions.Equal("Bearer synthetic-access", r.Header.Get("Authorization"))
			body = `{"email":"person@example.com"}`
		default:
			return nil, fmt.Errorf("unexpected OAuth request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	begin := func() CardDAVGoogleAuthorizeResponse {
		start := httptest.NewRequest(http.MethodPost, "https://archive.example/api/v1/carddav/google/authorize", strings.NewReader(`{"email":"person@example.com","redirect_uri":"https://archive.example/"}`))
		start.Header.Set("Content-Type", "application/json")
		start.Header.Set("Origin", "https://archive.example")
		started := httptest.NewRecorder()
		srv.Router().ServeHTTP(started, start)
		required.Equal(http.StatusOK, started.Code, started.Body.String())
		var flow CardDAVGoogleAuthorizeResponse
		required.NoError(json.Unmarshal(started.Body.Bytes(), &flow))
		return flow
	}
	older := begin()
	flow := begin()
	callback := httptest.NewRequest(http.MethodPost, "https://archive.example/api/v1/carddav/google/callback", strings.NewReader(fmt.Sprintf(`{"state":%q,"code":"synthetic-code"}`, flow.State)))
	callback.Header.Set("Content-Type", "application/json")
	callback = callback.WithContext(context.WithValue(callback.Context(), oauth2.HTTPClient, provider))
	completed := httptest.NewRecorder()
	srv.Router().ServeHTTP(completed, callback)
	required.Equal(http.StatusOK, completed.Code, completed.Body.String())
	assertions.Equal(1, reconciled, "successful authorization must reconcile the schedule without an account save")
	mgr, err := carddav.NewGoogleOAuthManager(secrets, cfg.TokensDir(), "", "person@example.com", testLogger())
	required.NoError(err)
	assertions.True(mgr.TokenMatchesClient("person@example.com"))
	assertions.True(mgr.HasScope("person@example.com", oauth.ScopeCardDAV))
	saved, err := os.ReadFile(mgr.TokenPath("person@example.com"))
	required.NoError(err)
	callback = httptest.NewRequest(http.MethodPost, "https://archive.example/api/v1/carddav/google/callback", strings.NewReader(fmt.Sprintf(`{"state":%q,"code":"synthetic-code"}`, older.State)))
	callback.Header.Set("Content-Type", "application/json")
	callback = callback.WithContext(context.WithValue(callback.Context(), oauth2.HTTPClient, provider))
	stale := httptest.NewRecorder()
	srv.Router().ServeHTTP(stale, callback)
	required.Equal(http.StatusBadRequest, stale.Code, stale.Body.String())
	assertions.Contains(stale.Body.String(), `"error":"oauth_changed"`)
	assertions.Contains(stale.Body.String(), "start sign-in again")
	assertions.Equal(1, reconciled, "failed authorization must not reconcile the schedule")
	afterStale, err := os.ReadFile(mgr.TokenPath("person@example.com"))
	required.NoError(err)
	assertions.Equal(saved, afterStale)
	holder, _, held := gate.Holder()
	assertions.True(held)
	assertions.Equal("archive operation", holder)
	unchangedMail, err := os.ReadFile(mailPath)
	required.NoError(err)
	assertions.Equal(mailToken, unchangedMail)
}
