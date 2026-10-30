package threads

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsOAuthFullCallback(t *testing.T) {
	const registered = "https://example.com/threads-callback?tenant=archive"
	const good = registered + "&code=code%2Bvalue&state=expected"
	cases := []struct {
		name, registered, returned, state string
		valid                             bool
	}{
		{"valid", registered, good, "expected", true},
		{"fragment", registered, good + "#_", "expected", true},
		{"missing state", registered, registered + "&code=secret", "expected", false},
		{"wrong state", registered, good, "other", false},
		{"empty expected", registered, good, "", false},
		{"duplicate state", registered, good + "&state=expected", "expected", false},
		{"duplicate code", registered, good + "&code=secret", "expected", false},
		{"duplicate registered", registered, good + "&tenant=archive", "expected", false},
		{"bare code", registered, "secret", "expected", false},
		{"denial", registered, good + "&error=access_denied&error_description=secret", "expected", false},
		{"origin", registered, strings.Replace(good, "example.com", "attacker.example", 1), "expected", false},
		{"port", registered, strings.Replace(good, "example.com", "example.com:444", 1), "expected", false},
		{"path", registered, strings.Replace(good, "/threads-callback", "/other", 1), "expected", false},
		{"query", registered, strings.Replace(good, "tenant=archive", "tenant=other", 1), "expected", false},
		{"extra query", registered, good + "&extra=1", "expected", false},
		{"userinfo", registered, strings.Replace(good, "https://", "https://secret@", 1), "expected", false},
		{"bad escape", registered, good + "&x=%zz", "expected", false},
		{"http", "http://example.com/callback", "http://example.com/callback?code=secret&state=expected", "expected", false},
		{"loopback", "https://127.0.0.1/callback", "https://127.0.0.1/callback?code=secret&state=expected", "expected", false},
		{"localhost", "https://localhost/callback", "https://localhost/callback?code=secret&state=expected", "expected", false},
		{"registered fragment", registered + "#_", good, "expected", false},
		{"registered reserved query", "https://example.com/cb?state=expected", good, "expected", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			code, err := ParseOAuthCallback(tt.registered, tt.returned, tt.state)
			if tt.valid {
				require.NoError(err)
				assert.Equal("code+value", code)
			} else {
				require.Error(err)
				assert.Empty(code)
				assert.NotContains(err.Error(), "secret")
				assert.NotContains(err.Error(), tt.returned)
			}
		})
	}
	t.Run("random state", func(t *testing.T) {
		assert := Assert.New(t)
		require := require.New(t)
		first, err := NewOAuthState()
		require.NoError(err)
		raw, err := base64.RawURLEncoding.DecodeString(first)
		require.NoError(err)
		assert.Len(raw, 32)
		second, err := NewOAuthState()
		require.NoError(err)
		assert.NotEqual(first, second)
	})
}

var oauthTestNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

const oauthTestOwner = "9007199254740993123"
const oauthTestCallback = "https://example.com/threads-callback?tenant=archive"

// This provider is an external boundary fixture: requests are parsed by a real
// HTTP server and must satisfy the documented exchange/debug contract.
type oauthFixture struct {
	server                       *httptest.Server
	calls                        atomic.Int32
	paths                        []string
	debugApp, debugOwner         string
	scopes                       []string
	valid                        bool
	issued, expires, dataExpires time.Time
	failPath                     string
	failStatus, failCode         int
	beforeRefresh                func()
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	f := &oauthFixture{debugApp: "123", debugOwner: oauthTestOwner, scopes: []string{"threads_basic", "threads_read_replies"}, valid: true, issued: oauthTestNow.Add(-8 * 24 * time.Hour), expires: oauthTestNow.Add(60 * 24 * time.Hour), dataExpires: oauthTestNow.Add(90 * 24 * time.Hour)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.paths = append(f.paths, r.URL.Path)
		Assert.NotContains(t, r.URL.Path, "v1.0")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == f.failPath {
			w.WriteHeader(f.failStatus)
			_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"provider-secret-body"}}`, f.failCode)
			return
		}
		switch r.URL.Path {
		case "/oauth/access_token":
			Assert.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, r.ParseForm())
			Assert.Empty(t, r.URL.RawQuery)
			Assert.Equal(t, "123", r.PostForm.Get("client_id"))
			Assert.Equal(t, "app-secret", r.PostForm.Get("client_secret"))
			Assert.Equal(t, "authorization-code", r.PostForm.Get("code"))
			Assert.Equal(t, "authorization_code", r.PostForm.Get("grant_type"))
			Assert.Equal(t, oauthTestCallback, r.PostForm.Get("redirect_uri"))
			_, _ = fmt.Fprintf(w, `{"access_token":"short-secret","user_id":%s}`, oauthTestOwner)
		case "/access_token":
			Assert.Equal(t, http.MethodGet, r.Method)
			Assert.Equal(t, "th_exchange_token", r.URL.Query().Get("grant_type"))
			Assert.Equal(t, "app-secret", r.URL.Query().Get("client_secret"))
			Assert.Equal(t, "short-secret", r.URL.Query().Get("access_token"))
			_, _ = fmt.Fprint(w, `{"access_token":"long-secret","token_type":"bearer","expires_in":5184000}`)
		case "/me":
			Assert.Equal(t, "id,username,name", r.URL.Query().Get("fields"))
			Assert.Equal(t, "long-secret", r.URL.Query().Get("access_token"))
			_, _ = fmt.Fprintf(w, `{"id":"%s","username":"example_owner","name":"Example Owner"}`, oauthTestOwner)
		case "/refresh_access_token":
			Assert.Equal(t, http.MethodGet, r.Method)
			Assert.Equal(t, "th_refresh_token", r.URL.Query().Get("grant_type"))
			Assert.Equal(t, "long-secret", r.URL.Query().Get("access_token"))
			Assert.Empty(t, r.URL.Query().Get("client_secret"))
			if f.beforeRefresh != nil {
				f.beforeRefresh()
			}
			_, _ = fmt.Fprint(w, `{"access_token":"refreshed-secret","token_type":"bearer","expires_in":5184000}`)
		case "/debug_token":
			if r.URL.Query().Get("input_token") == "long-secret" {
				Assert.Equal(t, "TH|123|app-secret", r.URL.Query().Get("access_token"))
			} else {
				Assert.Equal(t, "refreshed-secret", r.URL.Query().Get("access_token"))
			}
			Assert.Contains(t, []string{"long-secret", "refreshed-secret"}, r.URL.Query().Get("input_token"))
			body := map[string]any{"data": map[string]any{"app_id": f.debugApp, "user_id": f.debugOwner, "is_valid": f.valid, "scopes": f.scopes, "issued_at": f.issued.Unix(), "expires_at": f.expires.Unix(), "data_access_expires_at": f.dataExpires.Unix()}}
			if f.debugApp == "" {
				delete(body["data"].(map[string]any), "app_id")
			}
			data, err := json.Marshal(body)
			require.NoError(t, err)
			_, err = w.Write(data)
			Assert.NoError(t, err)
		default:
			Assert.Fail(t, "unexpected OAuth endpoint")
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *oauthFixture) options() OAuthClientOptions {
	return OAuthClientOptions{HTTPClient: f.server.Client(), OAuthBaseURL: f.server.URL, Now: func() time.Time { return oauthTestNow }}
}
func testOAuthRequest() OAuthRequest {
	return NewOAuthRequest("123", oauthTestCallback, "authorization-code", "app-secret", []string{"threads_basic", "threads_read_replies"})
}

func TestThreadsTokenLifecycleExchange(t *testing.T) {
	t.Run("verified long token", func(t *testing.T) {
		assert := Assert.New(t)
		f := newOAuthFixture(t)
		r, err := ExchangeCredential(context.Background(), f.options(), testOAuthRequest())
		require.NoError(t, err)
		assert.Equal("long-secret", r.AccessToken())
		assert.Equal("123", r.AppID)
		assert.Equal(oauthTestOwner, r.OwnerID)
		assert.Equal("example_owner", r.Username)
		assert.Equal(f.scopes, r.Scopes)
		assert.Equal(f.issued, r.IssuedAt)
		assert.Equal(f.dataExpires, r.DataAccessExpiresAt)
		assert.Equal(oauthTestCallback, r.RedirectURI)
		assert.Equal([]string{"/oauth/access_token", "/access_token", "/me", "/debug_token"}, f.paths)
	})
	t.Run("documented debug response omits app ID", func(t *testing.T) {
		f := newOAuthFixture(t)
		f.debugApp = ""
		r, err := ExchangeCredential(context.Background(), f.options(), testOAuthRequest())
		require.NoError(t, err)
		Assert.Equal(t, "123", r.AppID)
	})
	cases := []struct {
		name   string
		change func(*oauthFixture)
	}{
		{"wrong app", func(f *oauthFixture) { f.debugApp = "456" }},
		{"wrong owner", func(f *oauthFixture) { f.debugOwner = "789" }},
		{"missing scopes", func(f *oauthFixture) { f.scopes = []string{"threads_basic"} }},
		{"invalid", func(f *oauthFixture) { f.valid = false }},
		{"data expired", func(f *oauthFixture) { f.dataExpires = oauthTestNow }},
		{"token expired", func(f *oauthFixture) { f.expires = oauthTestNow }},
		{"missing data expiry", func(f *oauthFixture) { f.dataExpires = time.Unix(0, 0) }},
		{"future issuance", func(f *oauthFixture) { f.issued = oauthTestNow.Add(time.Hour) }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newOAuthFixture(t)
			tt.change(f)
			r, err := ExchangeCredential(context.Background(), f.options(), testOAuthRequest())
			require.Error(t, err)
			Assert.Empty(t, r.AccessToken())
		})
	}
	t.Run("invalid request before HTTP", func(t *testing.T) {
		f := newOAuthFixture(t)
		req := testOAuthRequest()
		req.RedirectURI = "http://example.com/cb"
		_, err := ExchangeCredential(context.Background(), f.options(), req)
		require.Error(t, err)
		Assert.Zero(t, f.calls.Load())
	})
	t.Run("invalid callback cannot enter exchange", func(t *testing.T) {
		f := newOAuthFixture(t)
		code, err := ParseOAuthCallback(oauthTestCallback, oauthTestCallback+"&code=authorization-code&state=wrong", "expected")
		require.Error(t, err)
		req := NewOAuthRequest("123", oauthTestCallback, code, "app-secret", nil)
		_, err = ExchangeCredential(context.Background(), f.options(), req)
		require.Error(t, err)
		Assert.Zero(t, f.calls.Load())
	})
}

func TestThreadsOAuthRedactionAndRedirect(t *testing.T) {
	assert := Assert.New(t)
	req := testOAuthRequest()
	r := testTokenRecord()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%p", "%d"} {
		for _, value := range []any{req, &req, r, &r} {
			formatted := fmt.Sprintf(verb, value)
			for _, secret := range []string{"app-secret", "authorization-code", "long-secret"} {
				assert.NotContains(formatted, secret)
			}
		}
	}
	for _, value := range []any{req, r} {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		assert.NotContains(string(data), "secret")
		assert.NotContains(string(data), "authorization-code")
	}
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	for _, status := range []int{302, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/leak", status) }))
			defer redirect.Close()
			opts := OAuthClientOptions{HTTPClient: redirect.Client(), OAuthBaseURL: redirect.URL}
			_, err := ExchangeCredential(context.Background(), opts, req)
			require.Error(t, err)
			assert.NotContains(err.Error(), target.URL)
			assert.Zero(targetCalls.Load())
		})
	}
	t.Run("URL error", func(t *testing.T) {
		assert := Assert.New(t)
		f := newOAuthFixture(t)
		opts := f.options()
		opts.HTTPClient = &http.Client{Transport: oauthFailTransport{}}
		_, err := ExchangeCredential(context.Background(), opts, req)
		require.Error(t, err)
		assert.NotContains(err.Error(), "secret")
		assert.NotContains(err.Error(), "https://")
		assert.ErrorIs(err, context.DeadlineExceeded)
	})
	t.Run("provider error", func(t *testing.T) {
		f := newOAuthFixture(t)
		f.failPath = "/access_token"
		f.failStatus = 400
		f.failCode = 190
		_, err := ExchangeCredential(context.Background(), f.options(), req)
		require.ErrorIs(t, err, ErrReauthorizationRequired)
		assert.NotContains(err.Error(), "secret")
	})
	t.Run("malformed provider JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, `{"access_token":"provider-secret-body",oops`)
		}))
		defer server.Close()
		_, err := ExchangeCredential(context.Background(), OAuthClientOptions{HTTPClient: server.Client(), OAuthBaseURL: server.URL}, req)
		require.Error(t, err)
		assert.NotContains(err.Error(), "secret")
	})
}

type oauthFailTransport struct{}

func (oauthFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Get", URL: "https://example.com/?access_token=transport-secret", Err: context.DeadlineExceeded}
}
