package inline

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// These fixtures exercise the network OAuth contract and the real loopback
// callback with synthetic delegated credentials. No browser or live provider
// account is needed.
type inlineOAuthServer struct {
	server           *httptest.Server
	mu               sync.Mutex
	registration     map[string]any
	authorize        url.Values
	grants           []url.Values
	metadataHook     func(map[string]any)
	registrationHook func(map[string]any)
	authorizeHook    func(http.ResponseWriter, *http.Request)
	tokenHook        func(http.ResponseWriter, *http.Request)
}

func newInlineOAuthServer(t *testing.T) *inlineOAuthServer {
	t.Helper()
	f := &inlineOAuthServer{}
	mux := http.NewServeMux()
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	base := f.server.URL
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		v := map[string]any{
			"issuer":                           base,
			"authorization_endpoint":           base + "/oauth/authorize",
			"token_endpoint":                   base + "/oauth/token",
			"registration_endpoint":            base + "/oauth/register",
			"scopes_supported":                 []string{"messages:read", "messages:write", "spaces:read", "offline_access"},
			"code_challenge_methods_supported": []string{"S256"},
		}
		if f.metadataHook != nil {
			f.metadataHook(v)
		}
		inlineOAuthWriteJSON(t, w, v)
	})
	mux.HandleFunc("/oauth/register", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		data, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request map[string]any
		if !assert.NoError(t, json.Unmarshal(data, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.registration = request
		f.mu.Unlock()
		response := map[string]any{
			"client_id":                  "synthetic-msgvault-client",
			"token_endpoint_auth_method": "none",
			"redirect_uris":              request["redirect_uris"],
		}
		if f.registrationHook != nil {
			f.registrationHook(response)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		inlineOAuthWriteJSON(t, w, response)
	})
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authorize = r.URL.Query()
		f.mu.Unlock()
		if f.authorizeHook != nil {
			f.authorizeHook(w, r)
			return
		}
		callback := r.URL.Query().Get("redirect_uri") + "?code=synthetic-code&state=" + url.QueryEscape(r.URL.Query().Get("state"))
		http.Redirect(w, r, callback, http.StatusFound) //nolint:gosec // Exercise the registered loopback redirect in the synthetic OAuth flow.
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		if !assert.NoError(t, r.ParseForm()) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.grants = append(f.grants, r.PostForm)
		f.mu.Unlock()
		if f.tokenHook != nil {
			f.tokenHook(w, r)
			return
		}
		inlineOAuthWriteJSON(t, w, map[string]any{
			"access_token": "synthetic-access", "refresh_token": "synthetic-refresh",
			"token_type": "Bearer", "expires_in": 3600,
			"scope": "messages:read offline_access",
		})
	})
	return f
}

func inlineOAuthWriteJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if !assert.NoError(t, err) { //nolint:testifylint // This helper runs in HTTP handler goroutines; report the failure and return HTTP 500 without calling FailNow.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(data)
	assert.NoError(t, err)
}

func inlineOAuthManagerForTest(t *testing.T, f *inlineOAuthServer, tokensDir string) *OAuthManager {
	t.Helper()
	m := NewOAuthManager(f.server.URL+"/mcp/v2", tokensDir, nil)
	baseTransport := f.server.Client().Transport
	m.http = &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: inlineOAuthFixtureTransport(func(r *http.Request) (*http.Response, error) {
			// A broken endpoint-pinning check must fail locally, without sending
			// even a synthetic credential to a non-fixture destination.
			if r.URL.Scheme+"://"+r.URL.Host != f.server.URL {
				return nil, errors.New("unexpected OAuth fixture destination")
			}
			return baseTransport.RoundTrip(r)
		}),
	}
	m.redirectPort = "0"
	m.openBrowserFn = func(ctx context.Context, rawURL string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		// Follow the authorization server redirect into the actual loopback
		// listener, as a browser would, without opening an external process.
		resp, err := f.server.Client().Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		return nil
	}
	return m
}

type inlineOAuthFixtureTransport func(*http.Request) (*http.Response, error)

func (f inlineOAuthFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func inlineOAuthCredentialsForTest(m *OAuthManager) OAuthCredentials {
	issuer, _ := m.issuer()
	return OAuthCredentials{
		Version: credentialVersion, Endpoint: m.endpoint,
		ClientID: "synthetic-msgvault-client", Resource: MCPResource,
		AuthEndpoint: issuer + "/oauth/authorize", TokenEndpoint: issuer + "/oauth/token",
		Scopes: []string{"messages:read", "offline_access"},
		Token: oauth2.Token{
			AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", TokenType: "Bearer",
			Expiry: time.Now().Add(time.Hour).UTC(),
		},
	}
}

func inlineOAuthPayloadForTest(t *testing.T, credentials OAuthCredentials) []byte {
	t.Helper()
	payload, err := json.Marshal(credentials)
	require.NoError(t, err)
	return payload
}

func TestInlineOAuthAuthorizePayloadPKCELoopback(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineOAuthServer(t)
	tokensDir := t.TempDir()
	m := inlineOAuthManagerForTest(t, f, tokensDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payload, err := m.AuthorizePayload(ctx)
	requires.NoError(err)

	f.mu.Lock()
	registration, auth, grants := f.registration, f.authorize, slices.Clone(f.grants)
	f.mu.Unlock()
	requires.Len(grants, 1)
	assertions.Equal("msgvault", registration["client_name"])
	assertions.Equal("none", registration["token_endpoint_auth_method"])
	assertions.Equal("messages:read offline_access", registration["scope"])
	assertions.Equal([]any{"authorization_code", "refresh_token"}, registration["grant_types"])
	assertions.Equal([]any{"code"}, registration["response_types"])
	assertions.Equal([]any{auth.Get("redirect_uri")}, registration["redirect_uris"])
	callbackURL, err := url.Parse(auth.Get("redirect_uri"))
	requires.NoError(err)
	assertions.Equal("http", callbackURL.Scheme)
	assertions.Equal("127.0.0.1", callbackURL.Hostname())
	assertions.NotEmpty(callbackURL.Port())
	assertions.NotEqual("0", callbackURL.Port())
	assertions.Equal(inlineCallbackPath, callbackURL.Path)
	assertions.Equal("code", auth.Get("response_type"))
	assertions.Equal("synthetic-msgvault-client", auth.Get("client_id"))
	assertions.Equal("messages:read offline_access", auth.Get("scope"))
	assertions.Equal(MCPResource, auth.Get("resource"))
	assertions.Equal("S256", auth.Get("code_challenge_method"))
	assertions.NotEmpty(auth.Get("state"))
	grant := grants[0]
	assertions.Equal("authorization_code", grant.Get("grant_type"))
	assertions.Equal("synthetic-code", grant.Get("code"))
	assertions.Equal("synthetic-msgvault-client", grant.Get("client_id"))
	assertions.Equal(auth.Get("redirect_uri"), grant.Get("redirect_uri"))
	assertions.Equal(MCPResource, grant.Get("resource"))
	assertions.Empty(grant.Get("client_secret"))
	verifier := grant.Get("code_verifier")
	requires.NotEmpty(verifier)
	digest := sha256.Sum256([]byte(verifier))
	assertions.Equal(base64.RawURLEncoding.EncodeToString(digest[:]), auth.Get("code_challenge"))
	assertions.NotEqual(verifier, auth.Get("code_challenge"))
	assertions.Empty(auth.Get("code_verifier"))

	credentials, err := m.decodeCredentials(payload)
	requires.NoError(err)
	assertions.Equal("synthetic-access", credentials.Token.AccessToken)
	assertions.Equal("synthetic-refresh", credentials.Token.RefreshToken)
	assertions.Equal(MCPResource, credentials.Resource)
	assertions.Equal([]string{"messages:read", "offline_access"}, credentials.Scopes)
	entries, err := os.ReadDir(tokensDir)
	requires.NoError(err)
	assertions.Empty(entries, "setup returns the handoff document without acquiring token-file custody")
}

func TestInlineOAuthWrongStateDoesNotConsumeFlow(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineOAuthServer(t)
	var wrongStatus atomic.Int32
	f.authorizeHook = func(w http.ResponseWriter, r *http.Request) {
		callback := r.URL.Query().Get("redirect_uri")
		wrong, err := http.NewRequestWithContext(r.Context(), http.MethodGet, callback+"?state=synthetic-wrong-state&code=synthetic-attacker-code", nil)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp, err := f.server.Client().Do(wrong)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		wrongStatus.Store(int32(resp.StatusCode))
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		_ = resp.Body.Close()
		correct := callback + "?code=synthetic-code&state=" + url.QueryEscape(r.URL.Query().Get("state"))
		http.Redirect(w, r, correct, http.StatusFound) //nolint:gosec // Registered loopback callback in the synthetic OAuth flow.
	}
	m := inlineOAuthManagerForTest(t, f, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := m.AuthorizePayload(ctx)
	requires.NoError(err)
	assertions.EqualValues(http.StatusBadRequest, wrongStatus.Load())
	f.mu.Lock()
	grants := slices.Clone(f.grants)
	f.mu.Unlock()
	requires.Len(grants, 1)
	assertions.Equal("synthetic-code", grants[0].Get("code"))
}

func TestInlineOAuthRejectsUnconfirmedPublicRegistration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing callback", func(v map[string]any) { delete(v, "redirect_uris") }},
		{"different callback", func(v map[string]any) { v["redirect_uris"] = []string{"http://127.0.0.1:1/callback/other"} }},
		{"confidential client", func(v map[string]any) { v["token_endpoint_auth_method"] = "client_secret_basic" }},
		{"missing client", func(v map[string]any) { delete(v, "client_id") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newInlineOAuthServer(t)
			f.registrationHook = tc.mutate
			m := inlineOAuthManagerForTest(t, f, t.TempDir())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var browserOpened atomic.Bool
			m.openBrowserFn = func(context.Context, string) error { browserOpened.Store(true); cancel(); return nil }
			_, err := m.AuthorizePayload(ctx)
			require.ErrorContains(t, err, "did not confirm")
			assert.False(t, browserOpened.Load())
			f.mu.Lock()
			assert.Empty(t, f.grants)
			f.mu.Unlock()
		})
	}
}

func TestInlineOAuthRejectsUnsafeDiscovery(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"different issuer", func(v map[string]any) { v["issuer"] = "https://issuer.example.com" }},
		{"different authorization endpoint", func(v map[string]any) { v["authorization_endpoint"] = "https://issuer.example.com/oauth/authorize" }},
		{"different token endpoint", func(v map[string]any) { v["token_endpoint"] = "https://issuer.example.com/oauth/token" }},
		{"different registration endpoint", func(v map[string]any) { v["registration_endpoint"] = "https://issuer.example.com/oauth/register" }},
		{"plain PKCE", func(v map[string]any) { v["code_challenge_methods_supported"] = []string{"plain"} }},
		{"missing read scope", func(v map[string]any) { v["scopes_supported"] = []string{"offline_access"} }},
		{"missing offline scope", func(v map[string]any) { v["scopes_supported"] = []string{"messages:read"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			f := newInlineOAuthServer(t)
			f.metadataHook = tc.mutate
			m := inlineOAuthManagerForTest(t, f, t.TempDir())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var browserOpened atomic.Bool
			m.openBrowserFn = func(context.Context, string) error { browserOpened.Store(true); cancel(); return nil }
			_, err := m.AuthorizePayload(ctx)
			require.ErrorContains(t, err, "authorization server")
			assertions.False(browserOpened.Load())
			f.mu.Lock()
			assertions.Nil(f.registration)
			assertions.Empty(f.grants)
			f.mu.Unlock()
		})
	}
}

func TestInlineOAuthImportExportOwnerOnly(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	m := NewOAuthManager("", t.TempDir(), nil)
	credentials := inlineOAuthCredentialsForTest(m)
	identifier := "inline:synthetic-account-42"
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	assertions.True(m.HasToken(identifier))
	path := m.TokenPath(identifier)
	assertions.NotContains(path, identifier)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		requires.NoError(err)
		assertions.Equal(os.FileMode(0600), info.Mode().Perm())
		lockInfo, err := os.Stat(path + ".lock")
		requires.NoError(err)
		assertions.Equal(os.FileMode(0600), lockInfo.Mode().Perm())
	}
	payload, err := m.ExportCredentials(identifier)
	requires.NoError(err)
	var exported OAuthCredentials
	requires.NoError(json.Unmarshal(payload, &exported))
	assertions.Equal(credentials, exported)

	credentials.Token.AccessToken = "synthetic-replacement-access"
	credentials.Token.RefreshToken = "synthetic-replacement-refresh"
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	payload, err = m.ExportCredentials(identifier)
	requires.NoError(err)
	requires.NoError(json.Unmarshal(payload, &exported))
	assertions.Equal(credentials, exported)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		requires.NoError(err)
		assertions.Equal(os.FileMode(0600), info.Mode().Perm())
	}
}

func TestInlineOAuthRejectsUnsafeCredentialDocuments(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OAuthCredentials)
	}{
		{"different version", func(c *OAuthCredentials) { c.Version++ }},
		{"different MCP endpoint", func(c *OAuthCredentials) { c.Endpoint = "https://mcp.example.com/mcp/v2" }},
		{"different audience", func(c *OAuthCredentials) { c.Resource = DefaultMCPEndpoint }},
		{"different authorization endpoint", func(c *OAuthCredentials) { c.AuthEndpoint = "https://issuer.example.com/oauth/authorize" }},
		{"different token endpoint", func(c *OAuthCredentials) { c.TokenEndpoint = "https://issuer.example.com/oauth/token" }},
		{"missing client", func(c *OAuthCredentials) { c.ClientID = "" }},
		{"oversized client", func(c *OAuthCredentials) { c.ClientID = strings.Repeat("x", 513) }},
		{"missing read scope", func(c *OAuthCredentials) { c.Scopes = []string{"offline_access"} }},
		{"write scope escalation", func(c *OAuthCredentials) { c.Scopes = append(c.Scopes, "messages:write") }},
		{"unrequested space scope", func(c *OAuthCredentials) { c.Scopes = append(c.Scopes, "spaces:read") }},
		{"missing access token", func(c *OAuthCredentials) { c.Token.AccessToken = "" }},
		{"missing refresh token", func(c *OAuthCredentials) { c.Token.RefreshToken = "" }},
		{"nonbearer token", func(c *OAuthCredentials) { c.Token.TokenType = "Basic" }},
		{"unbounded token", func(c *OAuthCredentials) { c.Token.Expiry = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewOAuthManager("", t.TempDir(), nil)
			credentials := inlineOAuthCredentialsForTest(m)
			tc.mutate(&credentials)
			payload := inlineOAuthPayloadForTest(t, credentials)
			require.Error(t, m.ImportCredentials("inline:synthetic-account-42", payload))
			assert.False(t, m.HasToken("inline:synthetic-account-42"))
			_, err := m.HTTPClientFromPayload(context.Background(), payload)
			require.Error(t, err)
		})
	}
	t.Run("malformed and oversized", func(t *testing.T) {
		m := NewOAuthManager("", t.TempDir(), nil)
		require.ErrorContains(t, m.ImportCredentials("inline:synthetic-account-42", []byte("{broken")), "malformed")
		require.ErrorContains(t, m.ImportCredentials("inline:synthetic-account-42", []byte(strings.Repeat("x", (1<<20)+1))), "too large")
	})
}

func TestInlineOAuthPayloadClientFencesOriginAndPath(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	var MCPRequests, CDNRequests atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		CDNRequests.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cdn.Close)
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		MCPRequests.Add(1)
		assert.Equal(t, "/mcp/v2", r.URL.Path)
		assert.Equal(t, "Bearer synthetic-access", r.Header.Get("Authorization"))
		if r.URL.Query().Get("redirect") == "1" {
			http.Redirect(w, r, cdn.URL+"/media", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(resource.Close)
	m := NewOAuthManager(resource.URL+"/mcp/v2", t.TempDir(), nil)
	client, err := m.HTTPClientFromPayload(context.Background(), inlineOAuthPayloadForTest(t, inlineOAuthCredentialsForTest(m)))
	requires.NoError(err)
	resp, err := client.Get(m.endpoint)
	requires.NoError(err)
	assertions.Equal(http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	for _, target := range []string{cdn.URL + "/media", resource.URL + "/media", resource.URL + "/mcp", strings.Replace(m.endpoint, "http://", "http://synthetic-user@", 1)} {
		resp, err := client.Get(target)
		if resp != nil {
			_ = resp.Body.Close()
		}
		requires.ErrorContains(err, "only be sent to their MCP endpoint")
	}
	resp, err = client.Get(m.endpoint + "?redirect=1")
	requires.NoError(err)
	assertions.Equal(http.StatusFound, resp.StatusCode)
	_ = resp.Body.Close()
	assertions.EqualValues(2, MCPRequests.Load())
	assertions.Zero(CDNRequests.Load(), "bearer client must never follow a redirect to media")
	entries, err := os.ReadDir(m.tokensDir)
	requires.NoError(err)
	assertions.Empty(entries, "verification cannot persist or refresh a handoff credential")
}

func TestInlineOAuthExpiredPayloadCannotVerifyOrRefresh(t *testing.T) {
	f := newInlineOAuthServer(t)
	m := inlineOAuthManagerForTest(t, f, t.TempDir())
	credentials := inlineOAuthCredentialsForTest(m)
	credentials.Token.Expiry = time.Now().Add(-time.Hour)
	_, err := m.HTTPClientFromPayload(context.Background(), inlineOAuthPayloadForTest(t, credentials))
	require.ErrorContains(t, err, "expired")
	f.mu.Lock()
	assert.Empty(t, f.grants)
	f.mu.Unlock()
}

func TestInlineOAuthRefreshRotationAcrossManagers(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineOAuthServer(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRefresh)
	var calls atomic.Int32
	f.tokenHook = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "refresh_token", r.PostForm.Get("grant_type"))
		assert.Equal(t, "synthetic-refresh", r.PostForm.Get("refresh_token"))
		assert.Equal(t, "synthetic-msgvault-client", r.PostForm.Get("client_id"))
		assert.Equal(t, MCPResource, r.PostForm.Get("resource"))
		assert.Empty(t, r.PostForm.Get("client_secret"))
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		inlineOAuthWriteJSON(t, w, map[string]any{
			"access_token": "synthetic-rotated-access", "refresh_token": "synthetic-rotated-refresh",
			"token_type": "Bearer", "expires_in": 3600, "scope": "messages:read offline_access",
		})
	}
	tokensDir := t.TempDir()
	first := inlineOAuthManagerForTest(t, f, tokensDir)
	second := inlineOAuthManagerForTest(t, f, tokensDir)
	identifier := "inline:synthetic-account-42"
	credentials := inlineOAuthCredentialsForTest(first)
	requires.NoError(first.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	firstSource, err := first.TokenSource(context.Background(), identifier)
	requires.NoError(err)
	secondSource, err := second.TokenSource(context.Background(), identifier)
	requires.NoError(err)
	// Sources obtained before replacement must reload the stored credential;
	// neither may cache the earlier valid access or refresh token snapshot.
	credentials.Token.Expiry = time.Now().Add(-time.Hour)
	requires.NoError(first.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	type result struct {
		token *oauth2.Token
		err   error
	}
	results := make(chan result, 2)
	go func() { token, err := firstSource.Token(); results <- result{token, err} }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		requires.NoError(ctx.Err())
	}
	// The server is still holding the first refresh response. A separate
	// manager's credential operation must remain blocked by that owner until
	// its caller cancels. This proves contention rather than relying on the
	// scheduler to overlap Token calls. Only the first manager has started a
	// Token call, so this also rejects a lock confined to one manager instance.
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	_, blockedErr := second.TokenSource(blockedCtx, identifier)
	blockedCancel()
	requires.ErrorIs(blockedErr, context.DeadlineExceeded)
	go func() { token, err := secondSource.Token(); results <- result{token, err} }()
	releaseRefresh()
	for range 2 {
		select {
		case got := <-results:
			requires.NoError(got.err)
			requires.NotNil(got.token)
			assertions.Equal("synthetic-rotated-access", got.token.AccessToken)
			assertions.Equal("synthetic-rotated-refresh", got.token.RefreshToken)
		case <-ctx.Done():
			requires.NoError(ctx.Err())
		}
	}
	assertions.EqualValues(1, calls.Load(), "shared custody must serialize rotation and reload the winning credential")
	payload, err := second.ExportCredentials(identifier)
	requires.NoError(err)
	stored, err := second.decodeCredentials(payload)
	requires.NoError(err)
	assertions.Equal("synthetic-rotated-access", stored.Token.AccessToken)
	assertions.Equal("synthetic-rotated-refresh", stored.Token.RefreshToken)
	assertions.True(stored.Token.Valid())
	if runtime.GOOS != "windows" {
		info, err := os.Stat(first.TokenPath(identifier))
		requires.NoError(err)
		assertions.Equal(os.FileMode(0600), info.Mode().Perm())
	}
	_, err = firstSource.Token()
	requires.NoError(err)
	assertions.EqualValues(1, calls.Load())
}

func TestInlineOAuthRefreshPreservesOmittedRefreshToken(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineOAuthServer(t)
	f.tokenHook = func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-refresh", r.PostForm.Get("refresh_token"))
		inlineOAuthWriteJSON(t, w, map[string]any{
			"access_token": "synthetic-refreshed-access", "token_type": "Bearer", "expires_in": 3600,
		})
	}
	m := inlineOAuthManagerForTest(t, f, t.TempDir())
	credentials := inlineOAuthCredentialsForTest(m)
	credentials.Token.Expiry = time.Now().Add(-time.Hour)
	identifier := "inline:synthetic-account-42"
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	source, err := m.TokenSource(context.Background(), identifier)
	requires.NoError(err)
	token, err := source.Token()
	requires.NoError(err)
	assertions.Equal("synthetic-refreshed-access", token.AccessToken)
	assertions.Equal("synthetic-refresh", token.RefreshToken)
	payload, err := m.ExportCredentials(identifier)
	requires.NoError(err)
	stored, err := m.decodeCredentials(payload)
	requires.NoError(err)
	assertions.Equal("synthetic-refresh", stored.Token.RefreshToken)
	assertions.Equal([]string{"messages:read", "offline_access"}, stored.Scopes)
}

func TestInlineOAuthRefreshFailureDoesNotDiscloseProviderBody(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineOAuthServer(t)
	f.tokenHook = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := io.WriteString(w, `{"error":"invalid_grant","access_token":"synthetic-provider-secret-access","refresh_token":"synthetic-provider-secret-refresh"}`)
		assert.NoError(t, err)
	}
	m := inlineOAuthManagerForTest(t, f, t.TempDir())
	credentials := inlineOAuthCredentialsForTest(m)
	credentials.Token.Expiry = time.Now().Add(-time.Hour)
	identifier := "inline:synthetic-account-42"
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	before, err := m.ExportCredentials(identifier)
	requires.NoError(err)
	source, err := m.TokenSource(context.Background(), identifier)
	requires.NoError(err)
	token, err := source.Token()
	requires.ErrorContains(err, "HTTP 401")
	assertions.Nil(token)
	assertions.Contains(err.Error(), "add-inline")
	assertions.NotContains(err.Error(), "synthetic-provider-secret")
	assertions.NotContains(err.Error(), "invalid_grant")
	after, exportErr := m.ExportCredentials(identifier)
	requires.NoError(exportErr)
	assertions.Equal(before, after, "a failed rotation must retain the stored credential")
}

func TestInlineOAuthRejectsEscalatedRefreshScopes(t *testing.T) {
	requires := require.New(t)

	f := newInlineOAuthServer(t)
	f.tokenHook = func(w http.ResponseWriter, r *http.Request) {
		inlineOAuthWriteJSON(t, w, map[string]any{
			"access_token": "synthetic-rotated-access", "refresh_token": "synthetic-rotated-refresh",
			"token_type": "Bearer", "expires_in": 3600, "scope": "messages:read messages:write offline_access",
		})
	}
	m := inlineOAuthManagerForTest(t, f, t.TempDir())
	credentials := inlineOAuthCredentialsForTest(m)
	credentials.Token.Expiry = time.Now().Add(-time.Hour)
	identifier := "inline:synthetic-account-42"
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	before, err := m.ExportCredentials(identifier)
	requires.NoError(err)
	source, err := m.TokenSource(context.Background(), identifier)
	requires.NoError(err)
	_, err = source.Token()
	requires.ErrorContains(err, "unrequested scope")
	after, err := m.ExportCredentials(identifier)
	requires.NoError(err)
	assert.Equal(t, before, after)
}

func TestInlineOAuthRemovalInvalidatesExistingTokenSource(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineOAuthServer(t)
	m := inlineOAuthManagerForTest(t, f, t.TempDir())
	identifier := "inline:synthetic-account-42"
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, inlineOAuthCredentialsForTest(m))))
	source, err := m.TokenSource(context.Background(), identifier)
	requires.NoError(err)
	token, err := source.Token()
	requires.NoError(err)
	assertions.Equal("synthetic-access", token.AccessToken)
	requires.NoError(m.DeleteToken(identifier))
	assertions.False(m.HasToken(identifier))
	token, err = source.Token()
	requires.Error(err)
	assertions.Nil(token)
	_, err = m.TokenSource(context.Background(), identifier)
	requires.ErrorContains(err, "add-inline")
	requires.NoError(m.DeleteToken(identifier), "removing absent credentials is idempotent")
	f.mu.Lock()
	assertions.Empty(f.grants)
	f.mu.Unlock()
}

func TestInlineOAuthHTTPClientObservesCredentialReplacementAndRemoval(t *testing.T) {
	requires := require.New(t)

	var mu sync.Mutex
	var bearerHeaders []string
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/mcp/v2", r.URL.Path)
		mu.Lock()
		bearerHeaders = append(bearerHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(resource.Close)
	m := NewOAuthManager(resource.URL+"/mcp/v2", t.TempDir(), nil)
	identifier := "inline:synthetic-account-42"
	credentials := inlineOAuthCredentialsForTest(m)
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	client, err := m.HTTPClient(context.Background(), identifier)
	requires.NoError(err)
	resp, err := client.Get(m.endpoint)
	requires.NoError(err)
	_ = resp.Body.Close()

	// Exercise the existing production client, not only its underlying source:
	// an oauth2 reuse wrapper must not hide a credential-custody change.
	credentials.Token.AccessToken = "synthetic-replacement-access"
	credentials.Token.RefreshToken = "synthetic-replacement-refresh"
	requires.NoError(m.ImportCredentials(identifier, inlineOAuthPayloadForTest(t, credentials)))
	resp, err = client.Get(m.endpoint)
	requires.NoError(err)
	_ = resp.Body.Close()
	requires.NoError(m.DeleteToken(identifier))
	resp, err = client.Get(m.endpoint)
	if resp != nil {
		_ = resp.Body.Close()
	}
	requires.Error(err, "existing clients must stop sending tokens once custody is removed")
	mu.Lock()
	assert.Equal(t, []string{"Bearer synthetic-access", "Bearer synthetic-replacement-access"}, bearerHeaders)
	mu.Unlock()
}
