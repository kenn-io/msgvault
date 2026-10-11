package microsoft

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	assert.Contains(got, scopeProfile)
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

func TestGraphManager_Authorize_ConfirmsMailboxViaProfile(t *testing.T) {
	upn := map[string]any{"preferred_username": "jdoe@example.org", "tid": "org-tenant-id"}
	primary := map[string]any{"email": "j.doe@example.com", "tid": "org-tenant-id"}
	absent := map[string]any{"tid": "org-tenant-id"}
	for _, product := range []struct {
		name       string
		newManager func(string, string, string, string, *slog.Logger) *GraphManager
	}{
		{"mail", NewGraphMailManager},
		{"mail write", NewGraphMailWriteManager},
		{"teams", NewGraphManager},
		{"contacts", NewGraphContactsManager},
	} {
		t.Run(product.name, func(t *testing.T) {
			for _, tc := range []struct {
				name                  string
				claims                map[string]any
				status                int
				body                  string
				saved                 bool
				invalid, missingToken bool
			}{
				{name: "mail", claims: upn, status: http.StatusOK, body: `{"mail":"John@example.com","userPrincipalName":"jdoe@example.org"}`, saved: true},
				{name: "smtp alias", claims: upn, status: http.StatusOK, body: `{"mail":"j.doe@example.com","proxyAddresses":["SMTP:j.doe@example.com","smtp:JOHN@example.com"]}`, saved: true},
				{name: "primary email versus alias", claims: primary, status: http.StatusOK, body: `{"mail":"j.doe@example.com","proxyAddresses":["SMTP:j.doe@example.com","smtp:john@example.com"]}`, saved: true},
				{name: "absent identity fields", claims: absent, status: http.StatusOK, body: `{"mail":"john@example.com"}`, saved: true},
				{name: "other mailbox", claims: upn, status: http.StatusOK, body: `{"mail":"bob@example.com","userPrincipalName":"bob@example.org","proxyAddresses":["SMTP:bob@example.com"]}`},
				{name: "other primary email", claims: primary, status: http.StatusOK, body: `{"mail":"bob@example.com"}`},
				{name: "absent identity and other mailbox", claims: absent, status: http.StatusOK, body: `{"mail":"bob@example.com"}`},
				{name: "non smtp proxy", claims: primary, status: http.StatusOK, body: `{"proxyAddresses":["SIP:john@example.com"]}`},
				{name: "forbidden", claims: upn, status: http.StatusForbidden, body: `{"error":{"code":"Authorization_RequestDenied"}}`},
				{name: "absent identity and forbidden", claims: absent, status: http.StatusForbidden, body: `{}`},
				{name: "invalid token", claims: absent, status: http.StatusOK, body: `{"mail":"john@example.com"}`, invalid: true},
				{name: "missing token", status: http.StatusOK, body: `{"mail":"john@example.com"}`, missingToken: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var calls atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						assert.Equal(t, "/me", r.URL.Path)
						assert.Equal(t, "mail,userPrincipalName,proxyAddresses", r.URL.Query().Get("$select"))
						assert.Equal(t, "Bearer new-access", r.Header.Get("Authorization"))
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(tc.body))
					}))
					t.Cleanup(srv.Close)
					m := product.newManager("test-client", "common", "", t.TempDir(), slog.Default())
					m.graphURL = srv.URL
					m.verifyIDTokenFn = testVerifyFn
					invalid := errors.New("invalid ID token signature or nonce")
					if tc.invalid {
						m.verifyIDTokenFn = func(context.Context, string) (*idTokenClaims, error) {
							return &idTokenClaims{Email: "john@example.com"}, invalid
						}
					}
					m.browserFlowFn = func(_ context.Context, _ string, scopes []string) (*oauth2.Token, string, error) {
						assert.Contains(t, scopes, scopeProfile)
						tok := &oauth2.Token{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer"}
						if !tc.missingToken {
							tok = tok.WithExtra(map[string]any{"id_token": makeScopedIDToken(t, tc.claims, scopes)})
						}
						return tok, "test-nonce", nil
					}
					old := &oauth2.Token{AccessToken: "old-access", RefreshToken: "old-refresh", TokenType: "Bearer"}
					require.NoError(t, m.saveToken("john@example.com", old, m.scopes, "old-tenant"))
					before, err := os.ReadFile(m.TokenPath("john@example.com"))
					require.NoError(t, err)

					err = m.Authorize(t.Context(), "john@example.com")
					if tc.saved {
						require.NoError(t, err)
						tf, err := m.loadTokenFile("john@example.com")
						require.NoError(t, err)
						assert.Equal(t, "new-access", tf.AccessToken)
						assert.Equal(t, "new-refresh", tf.RefreshToken)
						assert.Equal(t, "org-tenant-id", tf.TenantID)
					} else {
						require.Error(t, err)
						if tc.invalid {
							require.ErrorIs(t, err, invalid)
						} else if !tc.missingToken {
							assert.Contains(t, err.Error(), "sign in to the account that owns john@example.com")
							if tc.claims["email"] != nil || tc.claims["preferred_username"] != nil {
								var mismatch *TokenMismatchError
								require.ErrorAs(t, err, &mismatch)
							}
						}
						got, err := os.ReadFile(m.TokenPath("john@example.com"))
						require.NoError(t, err)
						assert.Equal(t, before, got, "existing credentials preserved")
					}
					wantCalls := int32(1)
					if tc.invalid || tc.missingToken {
						wantCalls = 0
					}
					assert.Equal(t, wantCalls, calls.Load())
				})
			}
		})
	}
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

func TestGraphManager_ExistingGrantWithoutProfile(t *testing.T) {
	for _, product := range []struct {
		name       string
		newManager func(string, string, string, string, *slog.Logger) *GraphManager
		permission string
	}{
		{"mail", NewGraphMailManager, scopeGraphMailRead},
		{"mail write", NewGraphMailWriteManager, scopeGraphMailReadWrite},
		{"teams", NewGraphManager, scopeGraphChatRead},
		{"contacts", NewGraphContactsManager, scopeGraphContactsReadWrite},
	} {
		t.Run(product.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			m := product.newManager("test-client", "common", "", t.TempDir(), nil)
			granted := slices.DeleteFunc(slices.Clone(m.scopes), func(scope string) bool { return scope == scopeProfile })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(r.ParseForm())
				assert.Equal("refresh_token", r.Form.Get("grant_type"))
				assert.Equal("old-refresh", r.Form.Get("refresh_token"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"fresh-access","token_type":"Bearer","expires_in":3600,"refresh_token":"fresh-refresh"}`))
			}))
			t.Cleanup(srv.Close)
			m.authorityURL = srv.URL
			token := &oauth2.Token{AccessToken: "old-access", RefreshToken: "old-refresh", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}
			require.NoError(m.saveToken("user@example.com", token, granted, "org-tenant-id"))
			ok, err := m.HasScopes("user@example.com")
			require.NoError(err)
			assert.True(ok)
			source, err := m.TokenSource(t.Context(), "user@example.com")
			require.NoError(err)
			access, err := source(t.Context())
			require.NoError(err)
			assert.Equal("fresh-access", access)
			saved, err := m.loadTokenFile("user@example.com")
			require.NoError(err)
			assert.ElementsMatch(granted, saved.Scopes)
			assert.Equal("fresh-refresh", saved.RefreshToken)

			missingPermission := slices.DeleteFunc(slices.Clone(granted), func(scope string) bool { return scope == product.permission })
			require.NoError(m.saveToken("user@example.com", token, missingPermission, "org-tenant-id"))
			ok, err = m.HasScopes("user@example.com")
			require.NoError(err)
			assert.False(ok)
			_, err = m.TokenSource(t.Context(), "user@example.com")
			require.ErrorContains(err, product.permission)
		})
	}
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

// Each Graph product's token lives in its own file with its own scope set, so
// another product's token never satisfies it, and its re-authorization hint
// names its own command.
func TestGraphManager_SeparateTokenAndScopes(t *testing.T) {
	type newManager func(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager
	for _, tc := range []struct {
		name        string
		manager     newManager
		file        string
		other       newManager
		otherScopes []string
		// otherWorks checks that the other product's token still serves it.
		otherWorks bool
		// wrongScopes is a grant that lacks this product's scope.
		wrongScopes []string
		wantScope   string
		wantHint    string
	}{
		{
			name: "mail", manager: NewGraphMailManager, file: "msmail_user@company.com.json",
			other: NewGraphManager, otherScopes: GraphScopes(), otherWorks: true,
			wrongScopes: []string{"https://graph.microsoft.com/User.Read", scopeOfflineAccess, "openid", scopeEmail},
			wantScope:   "https://graph.microsoft.com/Mail.Read", wantHint: "msgvault add-o365 user@company.com --graph",
		},
		{
			name: "contacts", manager: NewGraphContactsManager, file: "mscontacts_user@company.com.json",
			other: NewGraphMailManager, otherScopes: GraphMailScopes(),
			wrongScopes: GraphMailScopes(),
			wantScope:   "https://graph.microsoft.com/Contacts.ReadWrite", wantHint: "msgvault carddav authorize-microsoft user@company.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			otherMgr := tc.other("test-client", "common", "", dir, slog.Default())
			mgr := tc.manager("test-client", "common", "", dir, slog.Default())
			assert.Equal(filepath.Join(dir, tc.file), mgr.TokenPath("user@company.com"))

			token := &oauth2.Token{AccessToken: "graph-access", RefreshToken: "graph-refresh", TokenType: "Bearer"}
			require.NoError(otherMgr.saveToken("user@company.com", token, tc.otherScopes, "org-tid"))
			if tc.otherWorks {
				_, err := otherMgr.TokenSource(t.Context(), "user@company.com")
				require.NoError(err)
			}
			assert.False(mgr.HasToken("user@company.com"))

			require.NoError(mgr.saveToken("user@company.com", token, tc.wrongScopes, "org-tid"))
			_, err := mgr.TokenSource(t.Context(), "user@company.com")
			require.ErrorContains(err, tc.wantScope)
			require.ErrorContains(err, tc.wantHint)
		})
	}
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
