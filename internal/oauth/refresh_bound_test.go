package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// stubTransport is an inert RoundTripper for tests that never dial anything.
type stubTransport struct{}

func (stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}

// stubJar is an inert cookie jar used to prove shallow copies preserve it.
type stubJar struct{}

func (stubJar) SetCookies(*url.URL, []*http.Cookie) {}
func (stubJar) Cookies(*url.URL) []*http.Cookie     { return nil }

// The helper copies the caller's (or oauth2's default) client instead of
// mutating it, and never widens an existing timeout.
func TestWithRefreshHTTPClientCapsTimeout(t *testing.T) {
	t.Run("default client is copied and capped", func(t *testing.T) {
		assert := assert.New(t)

		ctx := withRefreshHTTPClient(context.Background())

		got, ok := ctx.Value(oauth2.HTTPClient).(*http.Client)
		require.True(t, ok, "context must carry a client")
		assert.Equal(refreshHTTPTimeout, got.Timeout, "capped timeout")
		assert.Equal(http.DefaultClient.Transport, got.Transport, "customized default transport preserved")
		assert.Equal(http.DefaultClient.Jar, got.Jar, "default jar preserved")
		assert.NotSame(http.DefaultClient, got, "global client must not be reused or mutated")
		assert.Zero(http.DefaultClient.Timeout, "http.DefaultClient must stay untouched")
	})

	t.Run("zero timeout is capped, settings preserved", func(t *testing.T) {
		assert := assert.New(t)

		transport := &stubTransport{}
		checkRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		caller := &http.Client{
			Transport:     transport,
			Jar:           stubJar{},
			CheckRedirect: checkRedirect,
		}

		ctx := withRefreshHTTPClient(context.WithValue(context.Background(), oauth2.HTTPClient, caller))

		got, ok := ctx.Value(oauth2.HTTPClient).(*http.Client)
		require.True(t, ok, "context must carry a client")
		assert.Equal(refreshHTTPTimeout, got.Timeout, "zero timeout capped")
		assert.Same(transport, got.Transport, "caller transport preserved")
		assert.Equal(caller.Jar, got.Jar, "caller jar preserved")
		assert.Equal(
			reflect.ValueOf(caller.CheckRedirect).Pointer(),
			reflect.ValueOf(got.CheckRedirect).Pointer(),
			"caller redirect policy preserved")
		assert.NotSame(caller, got, "caller client must not be mutated")
		assert.Zero(caller.Timeout, "caller timeout unchanged")
	})

	t.Run("long timeout is capped", func(t *testing.T) {
		caller := &http.Client{Timeout: 2 * time.Hour}

		ctx := withRefreshHTTPClient(context.WithValue(context.Background(), oauth2.HTTPClient, caller))

		got, ok := ctx.Value(oauth2.HTTPClient).(*http.Client)
		require.True(t, ok, "context must carry a client")
		assert.Equal(t, refreshHTTPTimeout, got.Timeout, "long timeout capped")
		assert.Equal(t, 2*time.Hour, caller.Timeout, "caller timeout unchanged")
	})

	t.Run("short timeout is kept as is", func(t *testing.T) {
		caller := &http.Client{Timeout: 5 * time.Second}

		ctx := withRefreshHTTPClient(context.WithValue(context.Background(), oauth2.HTTPClient, caller))

		got, ok := ctx.Value(oauth2.HTTPClient).(*http.Client)
		require.True(t, ok, "context must carry a client")
		assert.Same(t, caller, got, "already-bounded client is not replaced")
	})

	t.Run("earlier cancellation is preserved", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		derived := withRefreshHTTPClient(parent)
		cancel()
		assert.ErrorIs(t, derived.Err(), context.Canceled,
			"the derived context must honor the caller's cancellation")
	})
}

// stalledTokenTransport imitates a token endpoint that accepts the request
// and never responds: each round trip blocks until its request context is
// canceled, then surfaces the cancellation — the exact mechanism
// http.Client.Timeout uses to stop a stalled call.
type stalledTokenTransport struct {
	served chan *http.Request
}

func newStalledTokenTransport() *stalledTokenTransport {
	return &stalledTokenTransport{served: make(chan *http.Request, 4)}
}

func (s *stalledTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case s.served <- req:
	default:
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// tokenSourceForLaterRefresh builds a Manager token source whose stored
// access token is valid at creation (so creation performs no token-endpoint
// I/O) and then advances virtual time past its expiry. The returned source is
// the one oauth2.Transport would later drive through the contextless
// TokenSource.Token() — the exact path of the reported hang.
func tokenSourceForLaterRefresh(ctx context.Context, t *testing.T) oauth2.TokenSource {
	t.Helper()

	mgr := setupTestManager(t, Scopes)
	mgr.config.Endpoint = oauth2.Endpoint{TokenURL: "https://token.example.test/token"}
	writeTokenFile(t, mgr, "user@example.com", oauth2.Token{
		AccessToken:  "cached-access",
		RefreshToken: "refresh-1",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, Scopes)

	ts, err := mgr.TokenSource(ctx, "user@example.com")
	require.NoError(t, err, "TokenSource with valid cached token")

	time.Sleep(90 * time.Minute) // virtual clock: expire the cached access token
	return ts
}

// A token endpoint that never responds must not block a later Token() on an
// already-created source. The refresh runs through real http.Client timeout
// machinery over the production constant in virtual time; with the fix
// removed the bubble deadlocks and synctest fails the test.
func TestTokenSourceLaterRefreshBoundsStalledTokenEndpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)

		stall := newStalledTokenTransport()
		// Zero timeout: the cap must come from the production path.
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient,
			&http.Client{Transport: stall})

		ts := tokenSourceForLaterRefresh(ctx, t)

		start := time.Now()
		_, err := ts.Token()
		require.Error(err, "later refresh must surface the stalled endpoint")
		require.ErrorContains(err, "context deadline exceeded",
			"the unblock must come from the capped HTTP client")

		// x/oauth2 does not know this endpoint's auth style, so it probes
		// twice; each probe is individually capped at the production bound.
		elapsed := time.Since(start)
		assert.GreaterOrEqual(elapsed, 2*refreshHTTPTimeout,
			"both auth-style probes must run to their own bound")
		assert.Less(elapsed, 2*refreshHTTPTimeout+time.Second,
			"each probe must stop at the production bound")
		assert.Len(stall.served, 2, "one request per auth-style probe")
	})
}

// The cap lowers the ceiling, never the floor: a caller client already
// bounded tighter than the package cap keeps its own (virtual) timeout.
func TestTokenSourceLaterRefreshKeepsShorterCallerTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)

		stall := newStalledTokenTransport()
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient,
			&http.Client{Timeout: 5 * time.Second, Transport: stall})

		ts := tokenSourceForLaterRefresh(ctx, t)

		start := time.Now()
		_, err := ts.Token()
		require.Error(err, "later refresh must surface the stalled endpoint")
		require.ErrorContains(err, "context deadline exceeded",
			"the unblock must come from the caller's own timeout")
		elapsed := time.Since(start)
		assert.GreaterOrEqual(elapsed, 5*time.Second,
			"the caller's timeout is honored")
		assert.Less(elapsed, refreshHTTPTimeout,
			"a shorter caller timeout must not be widened to the package cap")
	})
}

// serveThenStallTransport answers token exchanges until told to stall, after
// which it behaves like stalledTokenTransport.
type serveThenStallTransport struct {
	served chan *http.Request
	stall  atomic.Bool
}

func newServeThenStallTransport() *serveThenStallTransport {
	return &serveThenStallTransport{served: make(chan *http.Request, 4)}
}

func (s *serveThenStallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case s.served <- req:
	default:
	}
	if s.stall.Load() {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"access_token":"sa-access","token_type":"Bearer","expires_in":3600}`)),
		Request: req,
	}, nil
}

// The service-account JWT exchange goes through the same capped client: the
// first Token() mints a token normally, and a later Token() on the
// already-created source after that token expires must surface a stalled
// token endpoint. The JWT flow is a single round trip per Token(), so one
// bounded call must suffice.
func TestServiceAccountTokenSourceBoundsLaterRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)

		transport := newServeThenStallTransport()
		// Zero timeout: the cap must come from the production path.
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient,
			&http.Client{Transport: transport})

		keyPath := filepath.Join(t.TempDir(), "service-account.json")
		writeServiceAccountKeyWithTokenURI(t, keyPath, 0600, "https://token.example.test/token")
		saMgr, err := NewServiceAccountManager(keyPath, Scopes)
		require.NoError(err, "NewServiceAccountManager")

		ts, err := saMgr.TokenSource(ctx, "user@example.com")
		require.NoError(err, "TokenSource")

		token, err := ts.Token()
		require.NoError(err, "first JWT exchange must succeed against a serving endpoint")
		assert.Equal("sa-access", token.AccessToken, "minted access token")
		assert.Len(transport.served, 1, "one serving round trip")

		transport.stall.Store(true)
		time.Sleep(90 * time.Minute) // virtual clock: expire the minted token

		start := time.Now()
		_, err = ts.Token()
		require.Error(err, "later JWT exchange must surface the stalled endpoint")
		require.ErrorContains(err, "context deadline exceeded",
			"the unblock must come from the capped HTTP client")
		elapsed := time.Since(start)
		assert.GreaterOrEqual(elapsed, refreshHTTPTimeout,
			"the later JWT exchange must run to the production bound")
		assert.Less(elapsed, 2*refreshHTTPTimeout,
			"the JWT exchange is a single capped round trip")
		assert.Len(transport.served, 2, "exactly one stalled round trip after expiry")
	})
}

// The ordinary refresh path keeps working against a real token endpoint:
// an expired stored token is redeemed through the bounded default client
// and the refreshed token is persisted.
func TestTokenSourceRefreshesExpiredTokenThroughBoundedClient(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		assert.NoError(r.ParseForm())
		assert.Equal("refresh_token", r.FormValue("grant_type"))
		assert.Equal("refresh-1", r.FormValue("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	mgr := setupTestManager(t, Scopes)
	mgr.config.Endpoint = oauth2.Endpoint{TokenURL: srv.URL}
	writeTokenFile(t, mgr, "user@example.com", oauth2.Token{
		AccessToken:  "old-access",
		RefreshToken: "refresh-1",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Minute),
	}, Scopes)

	ts, err := mgr.TokenSource(context.Background(), "user@example.com")
	require.NoError(err, "TokenSource should refresh the expired token")

	token, err := ts.Token()
	require.NoError(err, "Token")
	assert.Equal("new-access", token.AccessToken, "refreshed access token")
	assert.Equal("refresh-1", token.RefreshToken, "refresh token must survive the round trip")

	tf, err := mgr.loadTokenFile("user@example.com")
	require.NoError(err, "loadTokenFile")
	assert.Equal("new-access", tf.AccessToken, "refreshed token should be persisted")
	assert.Equal("refresh-1", tf.RefreshToken, "stored refresh token should be preserved")
	assert.Equal(Scopes, tf.Scopes, "stored scopes should be preserved")
}

// servingTokenTransport records each request it serves and answers with a
// fixed token response, proving which client handled the refresh.
type servingTokenTransport struct {
	served chan *http.Request
}

func (s *servingTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case s.served <- req:
	default:
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"access_token":"caller-access","token_type":"Bearer","expires_in":3600}`)),
		Request: req,
	}, nil
}

// A caller who supplies an oauth2.HTTPClient keeps their client: the refresh
// is served by that client's transport (the token URL is unroutable, so the
// package default could never serve it) with the refresh grant intact.
func TestTokenSourcePreservesCallerSuppliedHTTPClient(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	transport := &servingTokenTransport{served: make(chan *http.Request, 2)}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient,
		&http.Client{Transport: transport})

	mgr := setupTestManager(t, Scopes)
	mgr.config.Endpoint = oauth2.Endpoint{TokenURL: "https://token.invalid/token"}
	writeTokenFile(t, mgr, "user@example.com", oauth2.Token{
		AccessToken:  "expired-access",
		RefreshToken: "refresh-1",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Minute),
	}, Scopes)

	ts, err := mgr.TokenSource(ctx, "user@example.com")
	require.NoError(err, "caller-supplied client must handle the refresh")

	token, err := ts.Token()
	require.NoError(err, "Token")
	assert.Equal("caller-access", token.AccessToken, "refreshed access token")

	select {
	case req := <-transport.served:
		body, err := io.ReadAll(req.Body)
		require.NoError(err, "read refresh request body")
		form, err := url.ParseQuery(string(body))
		require.NoError(err, "parse refresh request body")
		assert.Equal("refresh_token", form.Get("grant_type"))
		assert.Equal("refresh-1", form.Get("refresh_token"))
	default:
		require.Fail("caller client never saw the refresh",
			"refresh was not routed to the caller-supplied client")
	}
}
