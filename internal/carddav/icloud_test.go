package carddav

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestICloudPartitionOrigins(t *testing.T) {
	for _, raw := range []string{"https://p42-contacts.icloud.com/home/", "https://P42-CONTACTS.ICLOUD.COM:443/home/"} {
		origin, ok := ICloudPartitionOrigin(raw)
		assert.True(t, ok, raw)
		assert.Equal(t, "https://p42-contacts.icloud.com", origin)
	}
	for _, raw := range []string{
		"http://p42-contacts.icloud.com/", "https://p42-contacts.icloud.com:8443/",
		"https://p42-contacts.icloud.com:/", "https://p42-contacts.icloud.com:0443/",
		"https://p42-contacts.icloud.com./", "https://p42-contacts.icloud.com.example.net/",
		"https://example.net/p42-contacts.icloud.com", "https://p42-contacts.icloud.com@other.example/",
		"https://user:secret@p42-contacts.icloud.com/", "https://p42-contacts.icloud.com/#fragment", // gitleaks:allow -- synthetic credential URI test cases
		"https://p42-contacts.icloud.com/?query", "https://p42-contacts.icloud.com/?",
		"https://p4-contacts.icloud.com/", "https://p-contacts.icloud.com/",
		"https://p٤٢-contacts.icloud.com/", "https://contacts.icloud.com/", "//p42-contacts.icloud.com/",
		" https://p42-contacts.icloud.com/", "https://p42-contacts.icloud.com\\@other.example/",
		"https://127.0.0.1/", "https://[::1]/", "https://p42-contacts.icloud.com:0/",
	} {
		_, ok := ICloudPartitionOrigin(raw)
		assert.False(t, ok, raw)
	}
}

func TestICloudRecognizersRejectLargeHost(t *testing.T) {
	raw := "https://p" + strings.Repeat("1", 100_000) + "-contacts.icloud.com/"
	_, ok := ICloudPartitionOrigin(raw)
	assert.False(t, ok)
	assert.False(t, IsICloudDiscoveryURL(raw))
}

func TestICloudPartitionCanonicalizationIsIdempotentProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Text())
		origin, ok := ICloudPartitionOrigin(raw)
		if !ok {
			assert.Empty(ht, origin)
			return
		}
		parsed, err := url.Parse(raw)
		require.NoError(ht, err)
		assert.Equal(ht, "https://"+strings.ToLower(parsed.Hostname()), origin)
		canonical, accepted := ICloudPartitionOrigin(origin)
		require.True(ht, accepted)
		assert.Equal(ht, origin, canonical)
	}, hegel.WithDatabase(""))
}

func TestICloudConflictingHomesNeverReceiveCredentials(t *testing.T) {
	var seen []string
	homes := "https://p42-contacts.icloud.com/synthetic/home/</D:href><D:href>https://p43-contacts.icloud.com/synthetic/home/"
	client := newICloudTLSFixture(t, iCloudFixtureHandler(t, homes, "", &seen))
	_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
	require.Error(t, err)
	assert.Len(t, seen, 2)
}

type iCloudCountedResponse struct {
	http.ResponseWriter

	bytes *atomic.Int64
}

func (w iCloudCountedResponse) Write(body []byte) (int, error) {
	w.bytes.Add(int64(len(body)))
	return w.ResponseWriter.Write(body)
}

func TestICloudBootstrapSharesByteBudgetAcrossOriginsProperty(t *testing.T) {
	var seen []string
	var emitted atomic.Int64
	handler := iCloudFixtureHandler(t, "https://p42-contacts.icloud.com/synthetic/home/", "", &seen)
	client := newICloudTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(iCloudCountedResponse{ResponseWriter: w, bytes: &emitted}, r)
	}))
	_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
	require.NoError(t, err)
	needed := emitted.Load()
	require.Positive(t, needed)
	check := func(checkT require.TestingT, limit int64) {
		clone := *client
		clone.operationBytes = limit
		_, discoveryErr := NewICloudService(nil, &clone).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
		if limit < needed {
			require.ErrorIs(checkT, discoveryErr, ErrOperationLimit)
		} else {
			require.NoError(checkT, discoveryErr)
		}
	}
	for _, limit := range []int64{needed - 1, needed, needed + 1} {
		check(t, limit)
	}
	hegel.Test(t, func(ht *hegel.T) {
		limit := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		check(ht, limit)
	}, hegel.WithTestCases(32), hegel.WithDatabase(""))
}

func TestICloudPartitionOriginNormalizationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		host := fmt.Sprintf("p%02d-contacts.icloud.com", n)
		authority := host
		if hegel.Draw(ht, hegel.Booleans()) {
			authority = strings.ToUpper(authority) + ":443"
		}
		origin, ok := ICloudPartitionOrigin("https://" + authority + "/synthetic/home/")
		require.True(ht, ok)
		assert.Equal(ht, "https://"+host, origin)
	}, hegel.WithDatabase(""))
}

func TestICloudRejectsArbitraryForeignAuthorityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		host := hegel.Draw(ht, hegel.Text())
		_, ok := ICloudPartitionOrigin("https://" + url.PathEscape(host) + ".example.net/home/")
		assert.False(ht, ok)
	}, hegel.WithDatabase(""))
}

func TestICloudPartitionDNSLabelBoundary(t *testing.T) {
	_, ok := ICloudPartitionOrigin("https://p" + strings.Repeat("1", 53) + "-contacts.icloud.com/")
	assert.True(t, ok)
	_, ok = ICloudPartitionOrigin("https://p" + strings.Repeat("1", 54) + "-contacts.icloud.com/")
	assert.False(t, ok)
	_, ok = ICloudPartitionOrigin("https://p42-contacts.icloud.com/#")
	assert.False(t, ok)
}

func TestDiscoveryDiagnosticDiscardsArbitraryPrivateComponentsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		private := hegel.Draw(ht, hegel.Text())
		target := &url.URL{Scheme: "https", Host: "other.example:443", Path: "/" + private, RawQuery: private, Fragment: private, User: url.UserPassword(private, private)}
		assert.Equal(ht, "Blocked server address: https://other.example:443", discoveryOriginError(target).Error())
	}, hegel.WithDatabase(""))
}

func TestICloudDiscoveryURLIsExactBootstrap(t *testing.T) {
	for _, raw := range []string{"https://contacts.icloud.com", "https://CONTACTS.ICLOUD.COM:443/"} {
		assert.True(t, IsICloudDiscoveryURL(raw), raw)
	}
	for _, raw := range []string{"http://contacts.icloud.com", "https://contacts.icloud.com:", "https://contacts.icloud.com:8443", "https://contacts.icloud.com/user/", "https://contacts.icloud.com/?", "https://user@contacts.icloud.com", "https://contacts.icloud.com.example.net", "https://contacts.icloud.com./"} {
		assert.False(t, IsICloudDiscoveryURL(raw), raw)
	}
}

// The server has synthetic certificates for these DNS names. DNS resolution
// and dial pinning still run; only the validated socket is routed locally.
func newICloudTLSFixture(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"contacts.icloud.com", "p42-contacts.icloud.com", "p43-contacts.icloud.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("203.0.113.9"))
	origin, err := url.Parse(ICloudDiscoveryURL)
	require.NoError(t, err)
	client, err := NewClient(ClientOptions{
		CredentialOrigin: origin, Username: "synthetic@example.net", Password: "synthetic-app-password", Resolver: resolver, // gitleaks:allow -- synthetic credential fixture
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			assert.Equal(t, "203.0.113.9:443", address)
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	})
	require.NoError(t, err)
	client.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return client
}

func iCloudFixtureHandler(t *testing.T, home string, extra string, seen *[]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.TrimSuffix(r.Host, ":443")
		*seen = append(*seen, host+r.URL.Path)
		username, password, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "synthetic@example.net", username)
		assert.Equal(t, "synthetic-app-password", password)
		switch host + r.URL.Path {
		case "contacts.icloud.com/":
			writeMultiStatus(t, w, `<D:response><D:href>/</D:href><D:propstat><D:prop><D:current-user-principal><D:href>/synthetic/principal/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "contacts.icloud.com/synthetic/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/synthetic/principal/</D:href><D:propstat><D:prop><C:addressbook-home-set><D:href>`+home+`</D:href></C:addressbook-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "p42-contacts.icloud.com/synthetic/home/":
			writeMultiStatus(t, w, `<D:response><D:href>/synthetic/home/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response><D:response><D:href>/synthetic/home/book/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><C:addressbook/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`+extra)
		default:
			assert.Fail(t, "unexpected credential destination", r.Host)
			w.WriteHeader(http.StatusForbidden)
		}
	})
}

func TestICloudRefusesCrossOriginRedirects(t *testing.T) {
	for _, destination := range []string{"https://other.example/sentinel-private/", "https://p43-contacts.icloud.com/synthetic/"} {
		var requests int
		client := newICloudTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.Header().Set("Location", destination)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
		require.ErrorIs(t, err, ErrUnsafeRedirect)
		assert.Equal(t, 1, requests)
		assert.NotContains(t, err.Error(), "sentinel")
	}
}

// serial: replaces the process-wide slog logger.
func TestICloudOmitsUpstreamBodyFromDebugLogs(t *testing.T) {
	assert := assert.New(t)
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	client := newICloudTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sentinel-private-id sentinel-app-password", http.StatusBadRequest)
	}))
	_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
	require.Error(t, err)
	assert.Contains(logged.String(), "status=400")
	assert.NotContains(logged.String(), "sentinel")
	assert.NotContains(err.Error(), "sentinel")
}

func TestICloudDiscoveryRedactsTransportAndParserErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newICloudTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<D:multistatus xmlns:D="DAV:"><sentinel-private></wrong></D:multistatus>`))
	}))
	_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
	require.Error(err)
	assert.NotContains(err.Error(), "sentinel")
	cause := errors.New("sentinel-private-path")
	client.dialContext = func(context.Context, string, string) (net.Conn, error) { return nil, cause }
	_, err = NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
	require.ErrorIs(err, cause)
	assert.NotContains(err.Error(), "sentinel")
}

func TestICloudBootstrapSelectsSingleCredentialOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var seen []string
	client := newICloudTLSFixture(t, iCloudFixtureHandler(t, "https://p42-contacts.icloud.com:443/synthetic/home/", "", &seen))
	discovery, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
	require.NoError(err)
	require.Len(discovery.Books, 1)
	assert.Equal("https://p42-contacts.icloud.com:443/synthetic/home/book/", discovery.Books[0].URL.String())
	assert.Equal([]string{"contacts.icloud.com/", "contacts.icloud.com/synthetic/principal/", "p42-contacts.icloud.com/synthetic/home/"}, seen)
	assert.Equal("contacts.icloud.com", client.origin.Host, "bootstrap never mutates caller credential origin")
}

func TestICloudBootstrapRefusesUntrustedHomeBeforeRequest(t *testing.T) {
	for _, home := range []string{"https://other.example/sentinel-private-path/?sentinel-secret", "http://p42-contacts.icloud.com/synthetic/home/", "https://p42-contacts.icloud.com:8443/synthetic/home/"} {
		t.Run(home, func(t *testing.T) {
			var seen []string
			client := newICloudTLSFixture(t, iCloudFixtureHandler(t, home, "", &seen))
			_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
			require.ErrorIs(t, err, ErrUnsafeHref)
			assert.Len(t, seen, 2)
			assert.NotContains(t, err.Error(), "sentinel-")
		})
	}
}

func TestGenericDiscoveryRetainsSameOriginProtectionForICloud(t *testing.T) {
	var seen []string
	client := newICloudTLSFixture(t, iCloudFixtureHandler(t, "https://p42-contacts.icloud.com/synthetic/home/", "", &seen))
	_, err := Discover(t.Context(), client, ICloudDiscoveryURL)
	require.ErrorIs(t, err, ErrUnsafeHref)
	assert.Len(t, seen, 2, "generic discovery never sends credentials to the named partition")
}

func TestICloudRefusesCrossOriginCollectionMembers(t *testing.T) {
	var seen []string
	extra := `<D:response><D:href>https://other.example/sentinel-private/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><C:addressbook/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
	client := newICloudTLSFixture(t, iCloudFixtureHandler(t, "https://p42-contacts.icloud.com/synthetic/home/", extra, &seen))
	_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
	require.ErrorIs(t, err, ErrUnsafeHref)
	assert.Len(t, seen, 3)
	assert.NotContains(t, err.Error(), "sentinel")
}

func TestICloudRequiresCompleteCollectionListing(t *testing.T) {
	for _, extra := range []string{
		`<D:response><D:href>/synthetic/home/hidden/</D:href><D:status>HTTP/1.1 403 Forbidden</D:status></D:response>`,
		`<D:response><D:href>/synthetic/home/hidden/</D:href><D:propstat><D:prop><D:displayname>Hidden</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
	} {
		var seen []string
		client := newICloudTLSFixture(t, iCloudFixtureHandler(t, "https://p42-contacts.icloud.com/synthetic/home/", extra, &seen))
		_, err := NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
		require.Error(t, err)
	}
}

func TestDiscoveryErrorNamesOnlyRefusedOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newFixtureClient(t, "https://contacts.example", "", "")
	base, err := url.Parse("https://contacts.example/principal/")
	require.NoError(err)
	_, err = resolveDiscoveryHref(t.Context(), client, base, "https://sentinel-user:sentinel-pass@p42-contacts.icloud.com:443/sentinel-path/?sentinel-query#sentinel-fragment") // gitleaks:allow -- synthetic credential URI test input
	require.ErrorIs(err, ErrUnsafeHref)
	assert.NotContains(err.Error(), "sentinel")
	_, err = resolveDiscoveryHref(t.Context(), client, base, "https://p42-contacts.icloud.com:443/sentinel-path/?sentinel-query")
	require.ErrorIs(err, ErrUnsafeHref)
	assert.Contains(err.Error(), "https://p42-contacts.icloud.com:443")
	assert.NotContains(err.Error(), "sentinel")
}

func TestICloudRecognizersRejectUnicodeCaseAliases(t *testing.T) {
	assert := assert.New(t)
	assert.False(IsICloudDiscoveryURL("https://contact\u017f.icloud.com/"))
	_, ok := ICloudPartitionOrigin("https://p00-contacts.\u0130cloud.com/")
	assert.False(ok)
	origin, ok := ICloudPartitionOrigin("https://p00-contacts.icloud.com/contacts/\u03bb/")
	assert.True(ok, "only the authority requires ASCII")
	assert.Equal("https://p00-contacts.icloud.com", origin)
}
