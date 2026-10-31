package x

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func callbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func TestOAuthRejectsStateAndScopeMismatch(t *testing.T) {
	for _, scenario := range []string{"mismatch", "narrowed", "reused", "expired", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			var exchanges atomic.Int32
			var challenge atomic.Value
			challenge.Store("")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/2/users/me" {
					assert.Equal(t, "Bearer oauth-access", r.Header.Get("Authorization"))
					fmt.Fprint(w, `{"data":{"id":"1000000001","username":"example_user","name":"Example User","protected":false}}`)
					return
				}
				exchanges.Add(1)
				require.NoError(t, r.ParseForm())
				digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				assert.Equal(t, challenge.Load().(string), base64.RawURLEncoding.EncodeToString(digest[:]))
				fmt.Fprint(w, `{"access_token":"oauth-access","refresh_token":"oauth-refresh","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read offline.access"}`)
			}))
			defer server.Close()
			var clock atomic.Int64
			clock.Store(time.Now().Unix())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := OAuthOptions{ClientID: "example-client", ClientType: PublicClient, AllowSend: true, CallbackPort: callbackPort(t), AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", APIBaseURL: server.URL + "/2", Now: func() time.Time { return time.Unix(clock.Load(), 0) }}
			opts.OpenBrowser = func(_ context.Context, raw string) error {
				u, err := url.Parse(raw)
				require.NoError(t, err)
				q := u.Query()
				challenge.Store(q.Get("code_challenge"))
				assert.Equal(t, "S256", q.Get("code_challenge_method"))
				assert.NotEmpty(t, challenge.Load().(string))
				assert.Contains(t, q.Get("scope"), "tweet.write")
				callback := q.Get("redirect_uri")
				assert.Equal(t, "http://127.0.0.1:"+strconv.Itoa(opts.CallbackPort)+"/callback", callback)
				state := q.Get("state")
				require.NotEmpty(t, state)
				if scenario == "cancelled" {
					cancel()
					return nil
				}
				if scenario == "expired" {
					clock.Add(600)
				}
				if scenario == "mismatch" {
					state = "wrong"
				}
				target := callback + "?code=example-code&state=" + url.QueryEscape(state)
				resp, err := http.Get(target)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				if scenario == "mismatch" || scenario == "expired" {
					assert.Equal(t, 400, resp.StatusCode)
				} else {
					assert.Equal(t, 200, resp.StatusCode)
				}
				if scenario == "reused" {
					resp, err = http.Get(target)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
					assert.Equal(t, 400, resp.StatusCode)
				}
				return nil
			}
			record, err := Authorize(ctx, opts)
			switch scenario {
			case "mismatch", "expired", "cancelled":
				require.Error(t, err)
				assert.Zero(t, exchanges.Load())
			default:
				require.NoError(t, err)
				assert.Equal(t, []string{"tweet.read", "users.read", "offline.access"}, record.GrantedScopes)
				assert.NotContains(t, record.GrantedScopes, "tweet.write")
				assert.Equal(t, "1000000001", record.OwnerID)
				assert.Equal(t, int32(1), exchanges.Load())
			}
		})
	}
}

func TestOAuthExplicitClientAuthentication(t *testing.T) {
	for _, kind := range []ClientType{PublicClient, ConfidentialClient} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", kind, reject), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/2/users/me" {
						fmt.Fprint(w, `{"data":{"id":"1000000001","username":"example_user","name":"Example User","protected":false}}`)
						return
					}
					calls.Add(1)
					require.NoError(t, r.ParseForm())
					if kind == PublicClient {
						assert.Equal(t, "example-client", r.Form.Get("client_id"))
						assert.Empty(t, r.Header.Get("Authorization"))
					} else {
						id, secret, ok := r.BasicAuth()
						assert.True(t, ok)
						assert.Equal(t, "example-client", id)
						assert.Equal(t, "client-secret", secret)
						assert.Empty(t, r.Form.Get("client_id"))
					}
					if reject {
						w.WriteHeader(401)
						fmt.Fprint(w, `{"error":"invalid_client","error_description":"client-secret"}`)
						return
					}
					fmt.Fprint(w, `{"access_token":"oauth-access","refresh_token":"oauth-refresh","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read offline.access"}`)
				}))
				defer server.Close()
				opts := OAuthOptions{ClientID: "example-client", ClientType: kind, CallbackPort: callbackPort(t), AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", APIBaseURL: server.URL + "/2", ReadSecret: func(context.Context) (string, error) { return "client-secret", nil }}
				opts.OpenBrowser = func(_ context.Context, raw string) error {
					u, err := url.Parse(raw)
					require.NoError(t, err)
					assert.NotContains(t, u.Query().Get("scope"), "tweet.write")
					q := url.Values{"state": {u.Query().Get("state")}, "code": {"example-code"}}
					resp, err := http.Get(u.Query().Get("redirect_uri") + "?" + q.Encode())
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
					return nil
				}
				_, err := Authorize(t.Context(), opts)
				if reject {
					require.Error(t, err)
					assert.NotContains(t, err.Error(), "client-secret")
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, int32(1), calls.Load())
			})
		}
	}
}

func TestOAuthMissingGrantMetadataFailsClosed(t *testing.T) {
	for _, scope := range []string{"", "tweet.read offline.access"} {
		t.Run(scope, func(t *testing.T) {
			var meCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/2/users/me" {
					meCalls.Add(1)
					fmt.Fprint(w, `{"data":{"id":"1000000001","username":"example_user","name":"Example User","protected":false}}`)
					return
				}
				fmt.Fprintf(w, `{"access_token":"oauth-access","refresh_token":"oauth-refresh","token_type":"bearer","expires_in":7200,"scope":%q}`, scope)
			}))
			defer server.Close()
			opts := OAuthOptions{ClientID: "example-client", ClientType: PublicClient, CallbackPort: callbackPort(t), AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", APIBaseURL: server.URL + "/2"}
			opts.OpenBrowser = func(_ context.Context, raw string) error {
				u, err := url.Parse(raw)
				require.NoError(t, err)
				resp, err := http.Get(u.Query().Get("redirect_uri") + "?code=example&state=" + url.QueryEscape(u.Query().Get("state")))
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				return nil
			}
			record, err := Authorize(t.Context(), opts)
			require.Error(t, err)
			assert.Empty(t, record.AccessToken())
			assert.Zero(t, meCalls.Load())
		})
	}
}

func TestOAuthRequiresIssuedRefreshToken(t *testing.T) {
	var meCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/2/users/me" {
			meCalls.Add(1)
			fmt.Fprint(w, `{"data":{"id":"1000000001","username":"example_user","name":"Example User","protected":false}}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"oauth-access","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read offline.access"}`)
	}))
	defer server.Close()
	opts := OAuthOptions{ClientID: "example-client", ClientType: PublicClient, CallbackPort: callbackPort(t), AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", APIBaseURL: server.URL + "/2"}
	opts.OpenBrowser = func(_ context.Context, raw string) error {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		response, err := http.Get(u.Query().Get("redirect_uri") + "?code=example&state=" + url.QueryEscape(u.Query().Get("state")))
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return nil
	}
	record, err := Authorize(t.Context(), opts)
	require.Error(t, err)
	assert.Empty(t, record.RefreshToken())
	assert.Zero(t, meCalls.Load())
}

func TestAuthorizationDoesNotForwardTokenEndpointRedirect(t *testing.T) {
	var forwards atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwards.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		id, secret, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "example-client", id)
		assert.Equal(t, "client-secret", secret)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	opts := OAuthOptions{ClientID: "example-client", ClientType: ConfidentialClient, CallbackPort: callbackPort(t), AuthURL: origin.URL + "/authorize", TokenURL: origin.URL + "/token", APIBaseURL: origin.URL + "/2", ReadSecret: func(context.Context) (string, error) { return "client-secret", nil }}
	opts.OpenBrowser = func(_ context.Context, raw string) error {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		response, err := http.Get(u.Query().Get("redirect_uri") + "?code=example-code&state=" + url.QueryEscape(u.Query().Get("state")))
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return nil
	}
	_, err := Authorize(t.Context(), opts)
	require.Error(t, err)
	assert.Zero(t, forwards.Load())
}
