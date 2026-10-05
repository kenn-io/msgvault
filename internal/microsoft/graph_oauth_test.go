package microsoft

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestGraphTokenPath(t *testing.T) {
	dir := filepath.Join("tmp", "tokens")
	m := NewGraphManager("", "", "", dir, nil)
	assert.Equal(t, filepath.Join(dir, "teams_user@example.com.json"), m.TokenPath("user@example.com"))
}

func TestGraphScopes(t *testing.T) {
	assert := assert.New(t)
	got := GraphScopes()
	assert.Contains(got, "https://graph.microsoft.com/Chat.Read")
	assert.Contains(got, "https://graph.microsoft.com/ChannelMessage.Read.All")
	assert.Contains(got, "https://graph.microsoft.com/Team.ReadBasic.All")
	assert.Contains(got, "https://graph.microsoft.com/Channel.ReadBasic.All")
	assert.Contains(got, "https://graph.microsoft.com/User.Read")
	assert.Contains(got, "https://graph.microsoft.com/User.ReadBasic.All")
	assert.Contains(got, "https://graph.microsoft.com/TeamMember.Read.All")
	assert.Contains(got, "https://graph.microsoft.com/ChannelMember.Read.All")
	assert.Contains(got, scopeOfflineAccess)
}

func TestNewGraphManager_DefaultsTenant(t *testing.T) {
	m := NewGraphManager("client", "", "", "tmp/tokens", nil)
	assert.Equal(t, DefaultTenant, m.tenantID, "tenantID should default to common")
	require.NotNil(t, m.logger, "logger should default")
}

func TestGraphManager_SaveLoadHasToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	m := NewGraphManager("client", "common", "", dir, slog.Default())

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
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", "", dir, slog.Default())
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
	m := NewGraphManager("test-client", "common", "", dir, slog.Default())
	m.verifyIDTokenFn = testVerifyFn
	m.browserFlowFn = func(_ context.Context, _ string, _ []string) (*oauth2.Token, string, error) {
		idToken := makeIDToken(t, map[string]any{"email": "other@example.com"})
		tok := (&oauth2.Token{AccessToken: "x", TokenType: "Bearer"}).
			WithExtra(map[string]any{"id_token": idToken})
		return tok, "nonce", nil
	}
	err := m.Authorize(t.Context(), "user@company.com")
	require.Error(t, err, "expected mismatch error")
	mismatch := &TokenMismatchError{}
	assert.ErrorAs(t, err, &mismatch, "expected *TokenMismatchError")
}

func TestGraphManager_TokenSource_NoIMAPValidation(t *testing.T) {
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", "", dir, slog.Default())

	// Save a Graph token. There is no IMAP scope; the IMAP Manager would
	// reject this, but GraphManager must accept it.
	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	require.NoError(t, m.saveToken("user@company.com", token, GraphScopes(), "org-tid"))

	ts, err := m.TokenSource(t.Context(), "user@company.com")
	require.NoError(t, err)
	require.NotNil(t, ts, "TokenSource returned nil")
}

func TestGraphManager_TokenSource_StaleGraphScopesReturnsError(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", "", dir, slog.Default())

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

// TestGraphManager_TokenSource_MissingRosterScopesReturnsError covers tokens
// minted before channel imports read rosters. Without TeamMember.Read.All and
// ChannelMember.Read.All every roster fetch fails, and channel media then fails
// closed on every sync, so the account must be prompted to re-authorize rather
// than sync silently.
func TestGraphManager_TokenSource_MissingRosterScopesReturnsError(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", "", dir, slog.Default())

	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	previouslyShippedScopes := []string{
		"https://graph.microsoft.com/Chat.Read",
		"https://graph.microsoft.com/ChannelMessage.Read.All",
		"https://graph.microsoft.com/Team.ReadBasic.All",
		"https://graph.microsoft.com/Channel.ReadBasic.All",
		"https://graph.microsoft.com/User.Read",
		"https://graph.microsoft.com/User.ReadBasic.All",
		scopeOfflineAccess,
		"openid",
		scopeEmail,
	}
	require.NoError(m.saveToken("user@company.com", token, previouslyShippedScopes, "org-tid"))

	_, err := m.TokenSource(t.Context(), "user@company.com")
	require.Error(err, "expected missing Graph scope error")
	require.ErrorContains(err, "missing Microsoft Graph scopes")
	require.ErrorContains(err, "TeamMember.Read.All")
	require.ErrorContains(err, "ChannelMember.Read.All")
	require.ErrorContains(err, "msgvault add-teams user@company.com")
}

func TestGraphManager_TokenSource_MissingToken(t *testing.T) {
	m := NewGraphManager("test-client", "common", "", t.TempDir(), slog.Default())
	_, err := m.TokenSource(t.Context(), "nobody@example.com")
	require.Error(t, err, "expected error for missing token")
	assert.ErrorContains(t, err, "no valid token")
}

func TestGraphManager_TokenSource_Concurrent(t *testing.T) {
	dir := t.TempDir()
	m := NewGraphManager("test-client", "common", "", dir, slog.Default())
	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	require.NoError(t, m.saveToken("user@company.com", token, GraphScopes(), "org-tid"))

	fn, err := m.TokenSource(t.Context(), "user@company.com")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, _ = fn(t.Context())
		})
	}
	wg.Wait()
}

// Mail and Teams tokens live in separate files with separate scope sets, so a
// Teams token never satisfies the mail manager and the reverse.
func TestGraphMailManager_SeparateTokenAndScopes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	teamsMgr := NewGraphManager("test-client", "common", "", dir, slog.Default())
	mailMgr := NewGraphMailManager("test-client", "common", "", dir, slog.Default())
	assert.Equal(filepath.Join(dir, "msmail_user@company.com.json"), mailMgr.TokenPath("user@company.com"))

	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	require.NoError(teamsMgr.saveToken("user@company.com", token, GraphScopes(), "org-tid"))
	_, err := teamsMgr.TokenSource(t.Context(), "user@company.com")
	require.NoError(err)
	assert.False(mailMgr.HasToken("user@company.com"))

	withoutMail := []string{"https://graph.microsoft.com/User.Read", scopeOfflineAccess, "openid", scopeEmail}
	require.NoError(mailMgr.saveToken("user@company.com", token, withoutMail, "org-tid"))
	_, err = mailMgr.TokenSource(t.Context(), "user@company.com")
	require.ErrorContains(err, "https://graph.microsoft.com/Mail.Read")
	require.ErrorContains(err, "msgvault add-o365 user@company.com --graph")
}

// The write manager shares the mail token. It refuses a read-only grant, and
// the sync manager still accepts the escalated one.
func TestGraphMailWriteManager_Scopes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	readMgr := NewGraphMailManager("test-client", "common", "", dir, slog.Default())
	writeMgr := NewGraphMailWriteManager("test-client", "common", "", dir, slog.Default())
	assert.Equal(readMgr.TokenPath("user@company.com"), writeMgr.TokenPath("user@company.com"))

	token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
	require.NoError(readMgr.saveToken("user@company.com", token, GraphMailScopes(), "org-tid"))
	ok, err := writeMgr.HasScopes("user@company.com")
	require.NoError(err)
	assert.False(ok)
	_, err = writeMgr.TokenSource(t.Context(), "user@company.com")
	require.ErrorContains(err, "Mail.ReadWrite")

	require.NoError(writeMgr.saveToken("user@company.com", token, GraphMailWriteScopes(), "org-tid"))
	ok, err = writeMgr.HasScopes("user@company.com")
	require.NoError(err)
	assert.True(ok)
	_, err = writeMgr.TokenSource(t.Context(), "user@company.com")
	require.NoError(err)
	_, err = readMgr.TokenSource(t.Context(), "user@company.com")
	require.NoError(err)
}

func TestRefreshingAccessTokenPersistsToEachManagerFileAndNamesProduct(t *testing.T) {
	release := make(chan struct{})
	var stall atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stall.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","token_type":"Bearer","expires_in":3600,"refresh_token":"r2"}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	imap := &Manager{clientID: "client", tenantID: DefaultTenant, tokensDir: t.TempDir(), logger: slog.Default(), authorityURL: srv.URL}
	graph := NewGraphManager("client", "common", "", t.TempDir(), slog.Default())
	graph.authorityURL = srv.URL
	expired := &oauth2.Token{AccessToken: "stale", RefreshToken: "r1", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}
	managers := []struct {
		product string
		path    string
		save    func() error
		source  func() (func(context.Context) (string, error), error)
	}{
		{"microsoft", imap.TokenPath("user@example.com"),
			func() error { return imap.saveToken("user@example.com", expired, nil, "") },
			func() (func(context.Context) (string, error), error) {
				return imap.TokenSource(t.Context(), "user@example.com")
			}},
		{"microsoft graph", graph.TokenPath("user@example.com"),
			func() error { return graph.saveToken("user@example.com", expired, GraphScopes(), "") },
			func() (func(context.Context) (string, error), error) {
				return graph.TokenSource(t.Context(), "user@example.com")
			}},
	}
	for _, manager := range managers {
		t.Run(manager.product, func(t *testing.T) {
			require := require.New(t)
			stall.Store(false)
			require.NoError(manager.save())
			tokenFn, err := manager.source()
			require.NoError(err)
			token, err := tokenFn(t.Context())
			require.NoError(err)
			assert.Equal(t, "fresh", token)
			saved, err := readTokenFile(manager.path)
			require.NoError(err)
			assert.Equal(t, "fresh", saved.AccessToken)

			stall.Store(true)
			previous := tokenRefreshTimeout
			tokenRefreshTimeout = 50 * time.Millisecond
			t.Cleanup(func() { tokenRefreshTimeout = previous })
			require.NoError(manager.save())
			tokenFn, err = manager.source()
			require.NoError(err)
			_, err = tokenFn(t.Context())
			require.ErrorContains(err, manager.product+" token refresh timed out")
		})
	}
}
