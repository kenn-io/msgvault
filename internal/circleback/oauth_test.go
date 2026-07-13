package circleback

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// fakeAS is an httptest-backed OAuth authorization server + MCP resource:
// 401 challenge with resource_metadata, RFC 9728 PRM, RFC 8414 metadata,
// RFC 7591 registration, and authorize/token endpoints.
type fakeAS struct {
	srv *httptest.Server

	registrations atomic.Int32
	lastAuthorize url.Values
	lastToken     url.Values
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	f := &fakeAS{}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	base := f.srv.URL

	// The MCP endpoint: always 401 with a pointer at the PRM.
	mux.HandleFunc("/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, base+"/prm"))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/prm", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              base + "/api/mcp",
			"authorization_servers": []string{base},
			"scopes_supported":      []string{"meetings:read"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           base,
			"authorization_endpoint":           base + "/authorize",
			"token_endpoint":                   base + "/token",
			"registration_endpoint":            base + "/register",
			"jwks_uri":                         base + "/jwks",
			"response_types_supported":         []string{"code"},
			"grant_types_supported":            []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.registrations.Add(1)
		var meta map[string]any
		_ = json.NewDecoder(r.Body).Decode(&meta)
		meta["client_id"] = "cid_test123"
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, meta)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuthorize = r.URL.Query()
		redirect := r.URL.Query().Get("redirect_uri") +
			"?code=authcode-1&state=" + url.QueryEscape(r.URL.Query().Get("state"))
		http.Redirect(w, r, redirect, http.StatusFound) //nolint:gosec // fake AS echoes the test's own redirect_uri
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.lastToken = r.PostForm
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			writeJSON(w, map[string]any{
				"access_token": "at-1", "token_type": "Bearer",
				"refresh_token": "rt-1", "expires_in": 3600,
			})
		case "refresh_token":
			writeJSON(w, map[string]any{
				"access_token": "at-2", "token_type": "Bearer",
				"refresh_token": "rt-2", "expires_in": 3600,
			})
		default:
			http.Error(w, "unsupported grant", http.StatusBadRequest)
		}
	})
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// newTestManager wires a Manager at the fake AS's MCP endpoint whose
// "browser" is an HTTP client that follows the authorize redirect into the
// localhost callback.
func newTestManager(t *testing.T, f *fakeAS, port string) *Manager {
	t.Helper()
	m := NewManager(f.srv.URL+"/api/mcp", t.TempDir(), nil)
	m.redirectPort = port
	m.openBrowserFn = func(ctx context.Context, rawURL string) error {
		go func() {
			resp, err := http.Get(rawURL) //nolint:gosec // test hook fetching the fake AS authorize URL
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	return m
}

func TestAuthorize_FullFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("localhost listener flow not exercised on windows CI")
	}
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeAS(t)
	m := newTestManager(t, f, "18090")

	require.NoError(m.Authorize(context.Background(), "alice@example.com"))

	// Discovery → DCR happened once.
	assert.EqualValues(1, f.registrations.Load())

	// The authorize request carried PKCE + RFC 8707 resource + our client.
	assert.Equal("cid_test123", f.lastAuthorize.Get("client_id"))
	assert.NotEmpty(f.lastAuthorize.Get("code_challenge"))
	assert.Equal("S256", f.lastAuthorize.Get("code_challenge_method"))
	assert.Equal(m.endpoint, f.lastAuthorize.Get("resource"))
	assert.Equal("meetings:read", f.lastAuthorize.Get("scope"))

	// The exchange carried the verifier + resource.
	assert.Equal("authorization_code", f.lastToken.Get("grant_type"))
	assert.NotEmpty(f.lastToken.Get("code_verifier"))
	assert.Equal(m.endpoint, f.lastToken.Get("resource"))

	// Token file: 0600, all refresh state persisted.
	path := m.TokenPath("alice@example.com")
	info, err := os.Stat(path)
	require.NoError(err)
	assert.Equal(os.FileMode(0600), info.Mode().Perm())

	tf, err := m.loadTokenFile("alice@example.com")
	require.NoError(err)
	assert.Equal("at-1", tf.Token.AccessToken)
	assert.Equal("rt-1", tf.Token.RefreshToken)
	assert.Equal("cid_test123", tf.ClientID)
	assert.Equal(f.srv.URL+"/token", tf.TokenEndpoint)
	assert.Equal(m.endpoint, tf.Resource)
	assert.Equal([]string{"meetings:read"}, tf.Scopes)

	// Re-authorizing reuses the registered client instead of minting another.
	require.NoError(m.Authorize(context.Background(), "alice@example.com"))
	assert.EqualValues(1, f.registrations.Load(), "re-auth must reuse the persisted client_id")
}

func TestTokenSource_RefreshPersistsRotatedToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeAS(t)
	m := NewManager(f.srv.URL+"/api/mcp", t.TempDir(), nil)

	// Seed an expired token file directly.
	require.NoError(m.saveToken("default", &tokenFile{
		Token: oauth2.Token{
			AccessToken:  "at-old",
			RefreshToken: "rt-1",
			Expiry:       time.Now().Add(-time.Hour),
		},
		ClientID:      "cid_test123",
		TokenEndpoint: f.srv.URL + "/token",
		Resource:      m.endpoint,
	}))

	ts, err := m.TokenSource(context.Background(), "default")
	require.NoError(err)
	tok, err := ts.Token()
	require.NoError(err)
	assert.Equal("at-2", tok.AccessToken)

	// The refresh grant carried the resource indicator and client_id.
	assert.Equal("refresh_token", f.lastToken.Get("grant_type"))
	assert.Equal("rt-1", f.lastToken.Get("refresh_token"))
	assert.Equal(m.endpoint, f.lastToken.Get("resource"))
	assert.Equal("cid_test123", f.lastToken.Get("client_id"))

	// The rotated refresh token was persisted.
	tf, err := m.loadTokenFile("default")
	require.NoError(err)
	assert.Equal("rt-2", tf.Token.RefreshToken)
	assert.Equal("at-2", tf.Token.AccessToken)

	// A second Token() call returns the cached (now valid) token without
	// another grant.
	f.lastToken = nil
	tok2, err := ts.Token()
	require.NoError(err)
	assert.Equal("at-2", tok2.AccessToken)
	assert.Nil(f.lastToken, "valid token must not trigger a refresh grant")
}

func TestTokenSource_MissingFileIsActionable(t *testing.T) {
	require := require.New(t)
	m := NewManager("", t.TempDir(), nil)
	_, err := m.TokenSource(context.Background(), "nobody")
	require.Error(err)
	require.Contains(err.Error(), "add-circleback")
}

func TestHandlerAuthorizeIsActionable(t *testing.T) {
	require := require.New(t)
	m := NewManager("", t.TempDir(), nil)
	h := m.Handler("alice@example.com")
	err := h.Authorize(context.Background(), nil, &http.Response{Status: "401 Unauthorized", Body: http.NoBody})
	require.Error(err)
	require.Contains(err.Error(), "add-circleback alice@example.com")
}

func TestSanitizeIdentifier_Injective(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// Common identifiers pass through unchanged.
	assert.Equal("alice@example.com", sanitizeIdentifier("alice@example.com"))
	assert.Equal("default", sanitizeIdentifier("default"))

	// Identifiers that a lossy replacement would collapse onto the same
	// token file must map to distinct filenames.
	inputs := []string{"team/a", "team\\a", "team_a", "team%2Fa", "team\x00a", "team a", "..", "team/../a"}
	seen := map[string]string{}
	for _, in := range inputs {
		out := sanitizeIdentifier(in)
		if prev, ok := seen[out]; ok {
			require.Failf("identifier collision", "%q and %q both sanitize to %q", prev, in, out)
		}
		seen[out] = in
		assert.NotContains(out, "/", "no path separator may survive for %q", in)
		assert.NotContains(out, "\\", "no path separator may survive for %q", in)
	}

	// TokenPath stays inside the tokens dir for hostile identifiers.
	tokensDir := t.TempDir()
	m := NewManager("", tokensDir, nil)
	assert.Equal(tokensDir, filepath.Dir(m.TokenPath("../../etc/passwd")))
}
