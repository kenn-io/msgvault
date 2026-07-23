package api

import (
	"context"
	"fmt"
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

	"go.kenn.io/msgvault/internal/config"
)

// fakePNG is a minimal PNG signature — enough for a byte-identity check; the
// proxy never decodes image data.
var fakePNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3, 4}

// remoteImageSeams records every resolver and dialer interaction while
// routing all dials to a local test HTTP server, so SSRF tests can assert
// exactly which addresses were resolved and dialed without real DNS.
type remoteImageSeams struct {
	mu       sync.Mutex
	lookups  []string
	dials    []string
	answers  map[string][]netip.Addr
	upstream string
}

func (s *remoteImageSeams) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups = append(s.lookups, host)
	addrs, ok := s.answers[host]
	if !ok {
		return nil, fmt.Errorf("no fake DNS answer for %q", host)
	}
	return addrs, nil
}

func (s *remoteImageSeams) dial(_ context.Context, network, address string) (net.Conn, error) {
	s.mu.Lock()
	s.dials = append(s.dials, address)
	upstream := s.upstream
	s.mu.Unlock()
	if upstream == "" {
		return nil, fmt.Errorf("test dial to %s refused: no upstream configured", address)
	}
	conn, err := net.Dial(network, upstream)
	if err != nil {
		return nil, fmt.Errorf("dialing test upstream: %w", err)
	}
	return conn, nil
}

func (s *remoteImageSeams) dialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.dials)
}

func (s *remoteImageSeams) dialedAddresses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dials...)
}

func newRemoteImageTestServer(t *testing.T, seams *remoteImageSeams) *Server {
	t.Helper()
	srv, _ := newTestServerWithMockStore(t)
	srv.remoteImages = &remoteImageFetcher{
		lookupNetIP:  seams.lookup,
		dialContext:  seams.dial,
		maxBytes:     remoteImageMaxBytes,
		maxRedirects: remoteImageMaxRedirects,
	}
	return srv
}

func getRemoteImage(srv *Server, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/content/remote-image?url="+url.QueryEscape(target), nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

func TestRemoteImageRejectsProhibitedTargetsBeforeAnyNetworkUse(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		wantCode string
	}{
		{"loopback literal", "http://127.0.0.1/a.png", "prohibited_host"},
		{"loopback short form", "http://127.1/a.png", "prohibited_host"},
		{"rfc1918 ten", "http://10.0.0.8/a.png", "prohibited_host"},
		{"rfc1918 172", "http://172.16.0.9/a.png", "prohibited_host"},
		{"rfc1918 192", "http://192.168.1.10/router.png", "prohibited_host"},
		{"cgnat", "http://100.64.0.1/a.png", "prohibited_host"},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data", "prohibited_host"},
		{"unspecified", "http://0.0.0.0/a.png", "prohibited_host"},
		{"broadcastish reserved", "http://240.0.0.1/a.png", "prohibited_host"},
		{"ipv6 loopback", "http://[::1]/a.png", "prohibited_host"},
		{"ipv6 link-local", "http://[fe80::1]/a.png", "prohibited_host"},
		{"ipv6 ula", "http://[fd00::2]/a.png", "prohibited_host"},
		{"ipv4-mapped loopback", "http://[::ffff:127.0.0.1]/a.png", "prohibited_host"},
		{"ipv4-compatible loopback", "http://[::7f00:1]/a.png", "prohibited_host"},
		{"nat64 private literal", "http://[64:ff9b::10.0.0.5]/a.png", "prohibited_host"},
		{"nat64 local-use literal", "http://[64:ff9b:1::a]/a.png", "prohibited_host"},
		{"6to4 private literal", "http://[2002:a00:1::]/a.png", "prohibited_host"},
		{"teredo private literal", "http://[2001:0:1234:5678::f5ff:fffe]/a.png", "prohibited_host"},
		{"decimal ipv4 obfuscation", "http://2130706433/a.png", "prohibited_host"},
		{"hex ipv4 obfuscation", "http://0x7f000001/a.png", "prohibited_host"},
		{"octal ipv4 obfuscation", "http://017700000001/a.png", "prohibited_host"},
		{"localhost name", "http://localhost:9000/daemon.png", "prohibited_host"},
		{"localhost subdomain", "http://api.localhost/a.png", "prohibited_host"},
		{"mdns local", "http://printer.local/status.png", "prohibited_host"},
		{"internal suffix", "http://api.internal/a.png", "prohibited_host"},
		{"home arpa", "http://nas.home.arpa/a.png", "prohibited_host"},
		{"single label", "http://nas/share.png", "prohibited_host"},
		{"trailing dot reserved", "http://printer.local./a.png", "prohibited_host"},
		{"numeric final label", "http://images.7/a.png", "prohibited_host"},
		{"ftp scheme", "ftp://images.example/a.png", "invalid_url"},
		{"javascript scheme", "javascript:alert(1)", "invalid_url"},
		{"userinfo", "http://user:pass@images.example/a.png", "invalid_url"},
		{"empty host", "http:///a.png", "invalid_url"},
		{"overlong url", "http://images.example/" + strings.Repeat("a", remoteImageMaxURLBytes), "invalid_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			seams := &remoteImageSeams{answers: map[string][]netip.Addr{}}
			srv := newRemoteImageTestServer(t, seams)

			resp := getRemoteImage(srv, tc.url)

			assert.Equal(http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
			assert.Contains(resp.Body.String(), tc.wantCode)
			assert.Empty(seams.lookups, "prohibited target must be rejected before DNS")
			assert.Zero(seams.dialCount(), "prohibited target must never be dialed")
		})
	}
}

func TestRemoteImageMissingURLParameter(t *testing.T) {
	assert := assert.New(t)
	seams := &remoteImageSeams{answers: map[string][]netip.Addr{}}
	srv := newRemoteImageTestServer(t, seams)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/remote-image", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusBadRequest, resp.Code)
	assert.Contains(resp.Body.String(), "missing_url")
	assert.Zero(seams.dialCount())
}

// TestRemoteImageRejectsRebindingResolution is the DNS rebinding case the
// browser cannot detect: a public-looking hostname whose resolver answers
// include a private address. Every A/AAAA answer is validated, so a single
// private answer rejects the fetch before any dial.
func TestRemoteImageRejectsRebindingResolution(t *testing.T) {
	cases := []struct {
		name    string
		answers []netip.Addr
	}{
		{"private answer", []netip.Addr{netip.MustParseAddr("10.0.0.5")}},
		{"loopback answer", []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{"metadata answer", []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
		{"ipv6 ula answer", []netip.Addr{netip.MustParseAddr("fd00::2")}},
		{"mapped loopback answer", []netip.Addr{netip.MustParseAddr("::ffff:127.0.0.1")}},
		{"public then private answer", []netip.Addr{
			netip.MustParseAddr("203.0.113.7"),
			netip.MustParseAddr("192.168.1.10"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			seams := &remoteImageSeams{answers: map[string][]netip.Addr{
				"rebind.example": tc.answers,
			}}
			srv := newRemoteImageTestServer(t, seams)

			resp := getRemoteImage(srv, "http://rebind.example/pixel.png")

			assert.Equal(http.StatusBadGateway, resp.Code, "body: %s", resp.Body.String())
			assert.Contains(resp.Body.String(), "prohibited_destination")
			assert.Equal([]string{"rebind.example"}, seams.lookups)
			assert.Zero(seams.dialCount(), "rebinding name must never be dialed")
		})
	}
}

// TestProhibitedRemoteIPTranslatedIPv4 covers the IPv6 transition mechanisms
// that embed an IPv4 destination: NAT64 (RFC 6052), the RFC 8215 local-use
// translation space, 6to4 (RFC 3056), and Teredo (RFC 4380). A prohibited
// embedded IPv4 prohibits the IPv6 address; a public one passes — except in
// the local-use space, where the layout is unknown and the whole /48 is
// rejected.
func TestProhibitedRemoteIPTranslatedIPv4(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{"nat64 rfc1918", "64:ff9b::10.0.0.5", true},
		{"nat64 metadata", "64:ff9b::169.254.169.254", true},
		{"nat64 loopback", "64:ff9b::127.0.0.1", true},
		{"nat64 public", "64:ff9b::8.8.8.8", false},
		{"local-use base", "64:ff9b:1::", true},
		{"local-use with public-looking tail", "64:ff9b:1::8.8.8.8", true},
		{"6to4 rfc1918", "2002:a00:1::", true},
		{"6to4 public", "2002:808:808::", false},
		{"teredo private client", "2001:0:1234:5678::f5ff:fffe", true},
		{"teredo public client", "2001:0:1234:5678::f7f7:f7f7", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prohibitedRemoteIP(netip.MustParseAddr(tc.addr))
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRemoteImageRejectsTranslatedPrivateResolution: a DNS64 or malicious
// resolver can answer with an IPv6 transition address whose embedded IPv4
// destination is private, reaching the LAN through the translator. Every
// such answer must be rejected before any dial.
func TestRemoteImageRejectsTranslatedPrivateResolution(t *testing.T) {
	cases := []struct {
		name   string
		answer string
	}{
		{"nat64 private answer", "64:ff9b::10.0.0.5"},
		{"nat64 local-use answer", "64:ff9b:1::203.0.113.7"},
		{"6to4 private answer", "2002:a00:1::"},
		{"teredo private client answer", "2001:0:1234:5678::f5ff:fffe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			seams := &remoteImageSeams{answers: map[string][]netip.Addr{
				"nat64.example": {netip.MustParseAddr(tc.answer)},
			}}
			srv := newRemoteImageTestServer(t, seams)

			resp := getRemoteImage(srv, "http://nat64.example/pixel.png")

			assert.Equal(http.StatusBadGateway, resp.Code, "body: %s", resp.Body.String())
			assert.Contains(resp.Body.String(), "prohibited_destination")
			assert.Equal([]string{"nat64.example"}, seams.lookups)
			assert.Zero(seams.dialCount(), "translated private destination must never be dialed")
		})
	}
}

func TestRemoteImageHappyPathPinsValidatedAddress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.Header.Get("Cookie"), "no cookies may travel outbound")
		assert.Equal("image/*", r.Header.Get("Accept"))
		assert.Equal(remoteImageUserAgent, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(fakePNG)
	}))
	defer upstream.Close()

	seams := &remoteImageSeams{
		answers:  map[string][]netip.Addr{"images.example": {netip.MustParseAddr("203.0.113.7")}},
		upstream: upstream.Listener.Addr().String(),
	}
	srv := newRemoteImageTestServer(t, seams)

	resp := getRemoteImage(srv, "http://images.example/chart.png?token=synthetic")

	require.Equal(http.StatusOK, resp.Code, "body: %s", resp.Body.String())
	assert.Equal("image/png", resp.Header().Get("Content-Type"))
	assert.Equal("nosniff", resp.Header().Get("X-Content-Type-Options"))
	assert.Equal("no-store", resp.Header().Get("Cache-Control"))
	assert.Equal(fakePNG, resp.Body.Bytes())
	// The dial went to the validated resolved address, not to a re-resolution
	// of the hostname (TOCTOU rebinding between check and dial).
	assert.Equal([]string{"203.0.113.7:80"}, seams.dialedAddresses())
}

func TestRemoteImageRedirectToPrivateTargetIsRejected(t *testing.T) {
	assert := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://internal-service.example/secret.png")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	seams := &remoteImageSeams{
		answers: map[string][]netip.Addr{
			"images.example":           {netip.MustParseAddr("203.0.113.7")},
			"internal-service.example": {netip.MustParseAddr("10.0.0.5")},
		},
		upstream: upstream.Listener.Addr().String(),
	}
	srv := newRemoteImageTestServer(t, seams)

	resp := getRemoteImage(srv, "http://images.example/chart.png")

	assert.Equal(http.StatusBadGateway, resp.Code, "body: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "prohibited_destination")
	assert.Equal([]string{"203.0.113.7:80"}, seams.dialedAddresses(),
		"only the first validated hop may be dialed")
}

// TestRemoteImageRedirectToNAT64PrivateTargetIsRejected re-validates redirect
// hops against the transition-address rules: the second hop resolves to a
// NAT64 address embedding a private IPv4 destination and must be rejected
// without a second dial.
func TestRemoteImageRedirectToNAT64PrivateTargetIsRejected(t *testing.T) {
	assert := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://nat64-gateway.example/secret.png")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	seams := &remoteImageSeams{
		answers: map[string][]netip.Addr{
			"images.example":        {netip.MustParseAddr("203.0.113.7")},
			"nat64-gateway.example": {netip.MustParseAddr("64:ff9b::10.0.0.5")},
		},
		upstream: upstream.Listener.Addr().String(),
	}
	srv := newRemoteImageTestServer(t, seams)

	resp := getRemoteImage(srv, "http://images.example/chart.png")

	assert.Equal(http.StatusBadGateway, resp.Code, "body: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "prohibited_destination")
	assert.Equal([]string{"203.0.113.7:80"}, seams.dialedAddresses(),
		"only the first validated hop may be dialed")
}

func TestRemoteImageRedirectToPrivateLiteralIsRejected(t *testing.T) {
	assert := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer upstream.Close()

	seams := &remoteImageSeams{
		answers:  map[string][]netip.Addr{"images.example": {netip.MustParseAddr("203.0.113.7")}},
		upstream: upstream.Listener.Addr().String(),
	}
	srv := newRemoteImageTestServer(t, seams)

	resp := getRemoteImage(srv, "http://images.example/chart.png")

	assert.Equal(http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "prohibited_host")
	assert.Equal(1, seams.dialCount())
}

func TestRemoteImageRedirectChainIsCapped(t *testing.T) {
	assert := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://images.example"+r.URL.Path+"x")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	seams := &remoteImageSeams{
		answers:  map[string][]netip.Addr{"images.example": {netip.MustParseAddr("203.0.113.7")}},
		upstream: upstream.Listener.Addr().String(),
	}
	srv := newRemoteImageTestServer(t, seams)

	resp := getRemoteImage(srv, "http://images.example/a")

	assert.Equal(http.StatusBadGateway, resp.Code, "body: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "too_many_redirects")
	assert.Equal(remoteImageMaxRedirects+1, seams.dialCount(),
		"the chain must stop after the redirect cap")
}

func TestRemoteImageRejectsNonImageContent(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
	}{
		{"html", "text/html; charset=utf-8"},
		{"json", "application/json"},
		{"svg", "image/svg+xml"},
		{"missing content type", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.contentType == "" {
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", tc.contentType)
				}
				_, _ = w.Write([]byte("<html>not an image</html>"))
			}))
			defer upstream.Close()

			seams := &remoteImageSeams{
				answers:  map[string][]netip.Addr{"images.example": {netip.MustParseAddr("203.0.113.7")}},
				upstream: upstream.Listener.Addr().String(),
			}
			srv := newRemoteImageTestServer(t, seams)

			resp := getRemoteImage(srv, "http://images.example/page")

			assert.Equal(http.StatusUnsupportedMediaType, resp.Code, "body: %s", resp.Body.String())
			assert.Contains(resp.Body.String(), "unsupported_type")
			assert.NotContains(resp.Body.String(), "not an image",
				"fetched bytes must never be echoed on error")
		})
	}
}

func TestRemoteImageEnforcesByteCapWhileStreaming(t *testing.T) {
	assert := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(make([]byte, 2048))
	}))
	defer upstream.Close()

	seams := &remoteImageSeams{
		answers:  map[string][]netip.Addr{"images.example": {netip.MustParseAddr("203.0.113.7")}},
		upstream: upstream.Listener.Addr().String(),
	}
	srv := newRemoteImageTestServer(t, seams)
	srv.remoteImages.maxBytes = 1024

	resp := getRemoteImage(srv, "http://images.example/huge.png")

	assert.Equal(http.StatusBadGateway, resp.Code, "body: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "image_too_large")
}

func TestRemoteImageUpstreamErrorStatus(t *testing.T) {
	assert := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer upstream.Close()

	seams := &remoteImageSeams{
		answers:  map[string][]netip.Addr{"images.example": {netip.MustParseAddr("203.0.113.7")}},
		upstream: upstream.Listener.Addr().String(),
	}
	srv := newRemoteImageTestServer(t, seams)

	resp := getRemoteImage(srv, "http://images.example/missing.png")

	assert.Equal(http.StatusBadGateway, resp.Code, "body: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "upstream_error")
	assert.NotContains(resp.Body.String(), "gone")
}

func TestRemoteImageRequiresAuthentication(t *testing.T) {
	assert := assert.New(t)
	const key = "remote-image-test-key"
	srv := NewServer(
		&config.Config{Server: config.ServerConfig{APIPort: 8080, APIKey: key}},
		nil, nil, testLogger(),
	)
	seams := &remoteImageSeams{answers: map[string][]netip.Addr{}}
	srv.remoteImages = &remoteImageFetcher{
		lookupNetIP:  seams.lookup,
		dialContext:  seams.dial,
		maxBytes:     remoteImageMaxBytes,
		maxRedirects: remoteImageMaxRedirects,
	}

	resp := getRemoteImage(srv, "http://images.example/chart.png")

	assert.Equal(http.StatusUnauthorized, resp.Code, "body: %s", resp.Body.String())
	assert.Empty(seams.lookups, "unauthenticated requests must not reach the fetcher")
	assert.Zero(seams.dialCount())
}
