package chatwoot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chatwootMediaRoute struct {
	host    string
	port    string
	address string
	ip      netip.Addr
}

type chatwootMediaRouter struct {
	mu      sync.Mutex
	routes  map[*httptest.Server]chatwootMediaRoute
	byHost  map[string]chatwootMediaRoute
	byPort  map[string]chatwootMediaRoute
	lookups []string
	dials   []string
}

func newChatwootMediaRouter(t *testing.T, servers ...*httptest.Server) *chatwootMediaRouter {
	t.Helper()
	router := &chatwootMediaRouter{
		routes: map[*httptest.Server]chatwootMediaRoute{},
		byHost: map[string]chatwootMediaRoute{},
		byPort: map[string]chatwootMediaRoute{},
	}
	for index, server := range servers {
		_, port, err := net.SplitHostPort(server.Listener.Addr().String())
		require.NoError(t, err)
		route := chatwootMediaRoute{
			host: fmt.Sprintf("media%d.chatwoot.example", index+1), port: port,
			address: server.Listener.Addr().String(), ip: netip.AddrFrom4([4]byte{203, 0, 113, byte(7 + index)}),
		}
		router.routes[server] = route
		router.byHost[route.host] = route
		router.byPort[route.port] = route
	}
	return router
}

func (router *chatwootMediaRouter) attach(client *Client) {
	client.lookupMediaIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		router.mu.Lock()
		defer router.mu.Unlock()
		route, ok := router.byHost[host]
		if !ok {
			return nil, errors.New("unexpected synthetic media host")
		}
		router.lookups = append(router.lookups, host)
		return []netip.Addr{route.ip}, nil
	}
	client.dialMedia = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split pinned media address: %w", err)
		}
		router.mu.Lock()
		route, ok := router.byPort[port]
		if ok {
			router.dials = append(router.dials, address)
		}
		router.mu.Unlock()
		if !ok || host != route.ip.String() {
			return nil, errors.New("media connection was not pinned to its validated address")
		}
		return (&net.Dialer{}).DialContext(ctx, network, route.address)
	}
}

func (router *chatwootMediaRouter) url(t *testing.T, server *httptest.Server, path string) string {
	t.Helper()
	route, ok := router.routes[server]
	require.True(t, ok)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + net.JoinHostPort(route.host, route.port) + path
}

func (router *chatwootMediaRouter) observations() ([]string, []string) {
	router.mu.Lock()
	defer router.mu.Unlock()
	return append([]string(nil), router.lookups...), append([]string(nil), router.dials...)
}

func TestClientMediaRejectsDNSResolutionToPrivateAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, err := NewClient("https://chatwoot.example.com", 9, "synthetic-token")
	require.NoError(err)
	c.lookupMediaIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		assert.Equal("cdn.chatwoot.example", host)
		return []netip.Addr{netip.MustParseAddr("10.4.5.6")}, nil
	}
	dialed := false
	c.dialMedia = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}
	target, err := url.Parse("https://cdn.chatwoot.example/audio.ogg")
	require.NoError(err)
	_, err = c.validateMediaTarget(t.Context(), target)
	require.Error(err)
	assert.False(dialed)
}

func TestClientMediaAllowsOnlyPinnedPrivateConfiguredOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, err := NewClient("https://chatwoot.internal", 9, "synthetic-token")
	require.NoError(err)
	c.lookupMediaIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		assert.Contains([]string{"chatwoot.internal", "cdn.chatwoot.example"}, host)
		return []netip.Addr{netip.MustParseAddr("10.4.5.6")}, nil
	}
	target, err := url.Parse("https://chatwoot.internal/storage/audio.ogg")
	require.NoError(err)
	pinned, err := c.validateMediaTarget(t.Context(), target)
	require.NoError(err, "a private self-hosted media URL may use the exact configured Chatwoot origin")
	assert.Equal([]netip.AddrPort{netip.MustParseAddrPort("10.4.5.6:443")}, pinned)

	external, err := url.Parse("https://cdn.chatwoot.example/storage/audio.ogg")
	require.NoError(err)
	_, err = c.validateMediaTarget(t.Context(), external)
	require.Error(err, "a different origin cannot inherit the configured private-origin allowance")
}

func TestClientMediaRevalidatesAndPinsEveryRedirect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var router *chatwootMediaRouter
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.Header.Get("Api_access_token"))
		assert.Empty(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "synthetic public media")
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.Header.Get("Api_access_token"))
		assert.Empty(r.Header.Get("Authorization"))
		http.Redirect(w, r, router.url(t, second, "/audio.ogg"), http.StatusFound)
	}))
	defer first.Close()
	c, err := NewClient("https://chatwoot.example.com", 9, "synthetic-token")
	require.NoError(err)
	router = newChatwootMediaRouter(t, first, second)
	router.attach(c)
	body, _, _, err := c.OpenMedia(t.Context(), router.url(t, first, "/signed-audio"), 1<<20)
	require.NoError(err)
	payload, err := io.ReadAll(body)
	require.NoError(err)
	require.NoError(body.Close())
	assert.Equal("synthetic public media", string(payload))
	lookups, dials := router.observations()
	assert.Equal([]string{"media1.chatwoot.example", "media2.chatwoot.example"}, lookups, "each redirect hop must be independently resolved")
	assert.Len(dials, 2)
	assert.True(strings.HasPrefix(dials[0], "203.0.113.7:"))
	assert.True(strings.HasPrefix(dials[1], "203.0.113.8:"))
}
