package microsoft

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestGraphTokenPath(t *testing.T) {
	dir := filepath.Join("tmp", "tokens")
	m := &GraphManager{tokensDir: dir}
	assertpkg.Equal(t, filepath.Join(dir, "teams_user@example.com.json"), m.TokenPath("user@example.com"))
}

func TestGraphScopes(t *testing.T) {
	assert := assertpkg.New(t)
	got := GraphScopes()
	assert.Contains(got, "https://graph.microsoft.com/Chat.Read")
	assert.Contains(got, "https://graph.microsoft.com/ChannelMessage.Read.All")
	assert.Contains(got, "https://graph.microsoft.com/Team.ReadBasic.All")
	assert.Contains(got, "https://graph.microsoft.com/Channel.ReadBasic.All")
	assert.Contains(got, "https://graph.microsoft.com/User.Read")
	assert.Contains(got, "https://graph.microsoft.com/User.ReadBasic.All")
	assert.Contains(got, scopeOfflineAccess)
}

func TestNewGraphManager_DefaultsTenant(t *testing.T) {
	m := NewGraphManager("client", "", "tmp/tokens", nil)
	assertpkg.Equal(t, DefaultTenant, m.tenantID, "tenantID should default to common")
	requirepkg.NotNil(t, m.logger, "logger should default")
}

func TestGraphManager_SaveLoadHasToken(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dir := t.TempDir()
	m := NewGraphManager("client", "common", dir, slog.Default())

	assert.False(m.HasToken("user@example.com"), "HasToken false before save")

	token := &oauth2.Token{AccessToken: "a", RefreshToken: "r", TokenType: "Bearer"}
	require.NoError(m.saveToken("user@example.com", token, GraphScopes(), "tid-1"))

	assert.True(m.HasToken("user@example.com"), "HasToken true after save")

	tf, err := m.loadTokenFile("user@example.com")
	require.NoError(err)
	assert.Equal("a", tf.AccessToken, "AccessToken")
	assert.Equal("tid-1", tf.TenantID, "TenantID")
	assert.Contains(tf.Scopes, "https://graph.microsoft.com/Chat.Read", "Graph scope persisted")

	// The on-disk format must be loadable by the IMAP Manager's loader too.
	imap := &Manager{tokensDir: dir}
	imapTf, err := imap.loadTokenFile("user@example.com")
	require.Error(err, "IMAP Manager uses microsoft_ prefix, should not find teams_ file")
	_ = imapTf
}

func TestGraphManager_Authorize_PersistsGraphToken(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", dir, slog.Default())
	m.verifyIDTokenFn = testVerifyFn

	var gotScopes []string
	m.browserFlowFn = func(_ context.Context, email string, scopes []string) (*oauth2.Token, string, error) {
		gotScopes = scopes
		idToken := makeIDToken(t, map[string]any{"email": email, "tid": "org-tid"})
		tok := (&oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}).
			WithExtra(map[string]any{"id_token": idToken})
		return tok, "test-nonce", nil
	}

	require.NoError(m.Authorize(t.Context(), "user@company.com"))

	// Graph scopes requested (no IMAP scope correction logic).
	assert.Contains(gotScopes, "https://graph.microsoft.com/Chat.Read", "requested Graph scope")
	assert.NotContains(gotScopes, ScopeIMAPOrg, "must not request IMAP scope")

	tf, err := m.loadTokenFile("user@company.com")
	require.NoError(err)
	assert.Equal("graph-access", tf.AccessToken, "AccessToken")
	assert.Equal("org-tid", tf.TenantID, "TenantID persisted")
	assert.Contains(tf.Scopes, "https://graph.microsoft.com/Chat.Read", "Graph scope persisted")
}

func TestGraphManager_Authorize_Mismatch(t *testing.T) {
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", dir, slog.Default())
	m.verifyIDTokenFn = testVerifyFn
	m.browserFlowFn = func(_ context.Context, _ string, _ []string) (*oauth2.Token, string, error) {
		idToken := makeIDToken(t, map[string]any{"email": "other@example.com"})
		tok := (&oauth2.Token{AccessToken: "x", TokenType: "Bearer"}).
			WithExtra(map[string]any{"id_token": idToken})
		return tok, "nonce", nil
	}
	err := m.Authorize(t.Context(), "user@company.com")
	requirepkg.Error(t, err, "expected mismatch error")
	mismatch := &TokenMismatchError{}
	assertpkg.ErrorAs(t, err, &mismatch, "expected *TokenMismatchError")
}

func TestGraphManager_TokenSource_NoIMAPValidation(t *testing.T) {
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", dir, slog.Default())

	// Save a Graph token. There is no IMAP scope; the IMAP Manager would
	// reject this, but GraphManager must accept it.
	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	requirepkg.NoError(t, m.saveToken("user@company.com", token, GraphScopes(), "org-tid"))

	ts, err := m.TokenSource(t.Context(), "user@company.com")
	requirepkg.NoError(t, err)
	requirepkg.NotNil(t, ts, "TokenSource returned nil")
}

func TestGraphManager_TokenSource_StaleGraphScopesReturnsError(t *testing.T) {
	require := requirepkg.New(t)
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", dir, slog.Default())

	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	oldScopes := []string{
		"https://graph.microsoft.com/Chat.Read",
		"https://graph.microsoft.com/ChannelMessage.Read.All",
		"https://graph.microsoft.com/Team.ReadBasic.All",
		"https://graph.microsoft.com/Channel.ReadBasic.All",
		"https://graph.microsoft.com/User.Read",
		scopeOfflineAccess,
		"openid",
		scopeEmail,
	}
	require.NoError(m.saveToken("user@company.com", token, oldScopes, "org-tid"))

	_, err := m.TokenSource(t.Context(), "user@company.com")
	require.Error(err, "expected stale Graph scope error")
	require.ErrorContains(err, "missing Microsoft Graph scopes")
	require.ErrorContains(err, "User.ReadBasic.All")
	require.ErrorContains(err, "msgvault add-teams user@company.com")
}

func TestGraphManager_TokenSource_MissingToken(t *testing.T) {
	m := NewGraphManager("test-client", "common", t.TempDir(), slog.Default())
	_, err := m.TokenSource(t.Context(), "nobody@example.com")
	requirepkg.Error(t, err, "expected error for missing token")
	assertpkg.ErrorContains(t, err, "no valid token")
}

func TestGraphManager_TokenSource_Concurrent(t *testing.T) {
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", dir, slog.Default())
	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	requirepkg.NoError(t, m.saveToken("user@company.com", token, GraphScopes(), "org-tid"))

	fn, err := m.TokenSource(t.Context(), "user@company.com")
	requirepkg.NoError(t, err)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, _ = fn(t.Context())
		})
	}
	wg.Wait()
}
