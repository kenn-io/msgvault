package cmd

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func setupCommandOAuth(t *testing.T) (*oauth.Manager, *invocation) {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.OAuth.ClientSecretsCommand = testutil.SecretCommand(t, "client")
	cfg.OAuth.Tokens = config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(t, err)
	mgr, err := oauth.NewManagerWithCredentials(t.Context(), source, cfg.TokensDir(), cfg.OAuth.Tokens, nil, oauth.Scopes)
	require.NoError(t, err)
	return mgr, testInvocationWithConfig(cfg)
}
func putCommandToken(t *testing.T, state *invocation, email string, scopes []string) {
	t.Helper()
	cfg := state.cfg
	data, err := json.Marshal(map[string]any{"access_token": "example-access", "refresh_token": "example-refresh", "expiry": time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "client_id": "example-client", "scopes": scopes})
	require.NoError(t, err)
	require.NoError(t, oauth.NewTokenStore(cfg.TokensDir(), cfg.OAuth.Tokens).Write(t.Context(), email, data))
}
func TestCommandOAuthScopePreservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, state := setupCommandOAuth(t)
	cfg := state.cfg
	putCommandToken(t, state, "reader@example.com", []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly})
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(err)
	mgr, err := newAddAccountOAuthManager(source, "reader@example.com", state)
	require.NoError(err)
	// Manager scope choices are observable in the actual OAuth consent URL.
	flow, err := mgr.BeginWebAuthorization("reader@example.com", "https://archive.example/")
	require.NoError(err)
	authURL, err := url.Parse(flow.URL)
	require.NoError(err)
	assert.ElementsMatch([]string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly}, strings.Fields(authURL.Query().Get("scope")))
	cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "fail")
	_, err = newAddAccountOAuthManager(source, "reader@example.com", state)
	require.Error(err)
	_, err = newCalendarOAuthManager(source, "reader@example.com", state)
	assert.Error(err)
}
func TestCommandOAuthReadonlyAliasAndFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, state := setupCommandOAuth(t)
	cfg := state.cfg
	saveAddAccountFlags(t)
	readonlyGrant = true
	putCommandToken(t, state, "Reader@example.com", oauth.Scopes)
	var out bytes.Buffer
	require.Error(applyAddAccountGrantDecision(&out, mgr, "reader@example.com", ""))
	cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "fail")
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(err)
	failed, err := oauth.NewManagerWithCredentials(t.Context(), source, cfg.TokensDir(), cfg.OAuth.Tokens, nil, oauth.Scopes)
	require.NoError(err)
	assert.Error(applyAddAccountGrantDecision(&out, failed, "reader@example.com", ""))
}
func TestCommandOAuthSetupDiscovery(t *testing.T) {
	_, state := setupCommandOAuth(t)
	cfg := state.cfg
	assert.Equal(t, "msgvault add-account you@example.com", setupAddAccountCommand(&cfg.OAuth))
	cfg.OAuth.Apps = map[string]config.OAuthApp{"work": {ClientSecretsCommand: cfg.OAuth.ClientSecretsCommand}}
	cfg.OAuth.ClientSecretsCommand = nil
	assert.Equal(t, "msgvault add-account you@example.com --oauth-app 'work'", setupAddAccountCommand(&cfg.OAuth))
}

func TestCommandCalendarHeadlessInstructionsUseUploadOnlyExport(t *testing.T) {
	var out bytes.Buffer
	printCommandHeadlessInstructions(&out, "reader@example.com", "work", true, false)

	assert.Contains(t, out.String(), "msgvault export-token 'reader@example.com' --upload-only")
	assert.Contains(t, out.String(), "msgvault add-calendar 'reader@example.com' --oauth-app 'work'")
	assert.NotContains(t, out.String(), "msgvault add-account")
}

func TestCommandTokenExport(t *testing.T) {
	_, state := setupCommandOAuth(t)
	cfg := state.cfg
	putCommandToken(t, state, "reader@example.com", oauth.ScopesGmailReadonly)
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/token/reader@example.com" {
			uploaded, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	var out bytes.Buffer
	exporter := &tokenExporter{httpClient: server.Client(), tokensDir: cfg.TokensDir(), tokenCommands: cfg.OAuth.Tokens, stdout: &out, stderr: &out}
	_, err := exporter.export("reader@example.com", server.URL, "example-key", true)
	require.NoError(t, err)
	assert.Contains(t, string(uploaded), "example-refresh")
	_, err = os.Stat(filepath.Join(cfg.TokensDir(), "reader@example.com.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestCommandMetadataUsesSelectedSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, state := setupCommandOAuth(t)
	cfg := state.cfg
	putCommandToken(t, state, "reader@example.com", []string{oauth.ScopeGmailReadonly, "https://www.googleapis.com/auth/drive.readonly"})
	cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "read-once")
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(err)
	mgr, err := newCalendarOAuthManager(source, "reader@example.com", state)
	require.NoError(err)
	scopes := mgr.GrantedScopes("reader@example.com")
	got := calendarEscalationScopes(scopes, calendarShouldPreserveGmail(true, mgr.HasScopeMetadata("reader@example.com"), scopes))
	assert.ElementsMatch([]string{oauth.ScopeGmailReadonly, "https://www.googleapis.com/auth/drive.readonly", oauth.ScopeCalendarReadonly}, got)
	assert.False(addAccountTokenHasGmailScopes(mgr, "reader@example.com", false))
}

func TestCommandEscalationKeepsSelectionSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr, state := setupCommandOAuth(t)
	cfg := state.cfg
	putCommandToken(t, state, "reader@example.com", oauth.Scopes)
	info, err := mgr.InspectToken(t.Context(), "reader@example.com")
	require.NoError(err)
	putCommandToken(t, state, "reader@example.com", oauth.ScopesGmailReadonly)
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(err)
	// A changed grant must be rejected before browser consent begins.
	escalation, err := newScopeEscalationManager(withInvocation(t.Context(), state), "reader@example.com", oauth.ScopesGmailCalendar, source, info)
	require.NoError(err)
	_, err = escalation.BeginWebAuthorization("reader@example.com", "https://archive.example/")
	assert.ErrorIs(err, oauth.ErrTokenChanged)
}
