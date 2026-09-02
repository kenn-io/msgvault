package peoplesweep

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modelsDevUserAgent     = "OpenAI File Downloader, XaiImageApiFetch/1.0"
	modelsDevRequestCanary = "catalog-request-canary-never-send"
	modelsDevBodyCanary    = "catalog-body-canary-never-report"
)

func TestModelsDevFetchParsesCurrentFixtureDeterministicallyByAPIShape(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	fixture, err := os.ReadFile("testdata/modelsdev/catalog.json")
	require.NoError(err)
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert := newAssert(t)
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api.json", r.URL.Path)
		assert.Equal("models.dev", r.Host)
		assert.Equal(modelsDevUserAgent, r.UserAgent())
		assert.Empty(r.URL.RawQuery)
		assert.Equal(int64(0), r.ContentLength)
		for _, name := range []string{"Authorization", "Cookie", "X-API-Key", "X-Goog-API-Key", "X-Config", "X-Host-ID", "X-Archive"} {
			assert.Empty(r.Header.Get(name))
		}
		for name, values := range r.Header {
			for _, value := range values {
				assert.NotContains(name, modelsDevRequestCanary)
				assert.NotContains(value, modelsDevRequestCanary)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(server.Close)

	got, err := client.Fetch(t.Context())
	require.NoError(err)
	require.Len(got, 4)
	assert.Equal([]string{"alpha", "anthropic-label", "openai-label", "template"},
		[]string{got[0].ID, got[1].ID, got[2].ID, got[3].ID})
	assert.Equal([]string{"ALPHA_API_KEY", "ALPHA_TOKEN"}, got[0].EnvironmentNames)
	assert.Equal([]Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses}, got[0].ProtocolCandidates)
	assert.Equal([]Protocol{ProtocolOpenAIChat}, got[1].ProtocolCandidates)
	assert.Equal([]string{"302AI_API_KEY", "SHAPE_API_KEY"}, got[1].EnvironmentNames)
	assert.Empty(got[2].ProtocolCandidates)
	assert.Equal([]Protocol{ProtocolGoogleGenerateContent}, got[3].ProtocolCandidates)
	assert.Equal("${CATALOG_BASE_URL}/v1", got[3].Endpoint)

	require.Len(got[0].Models, 1)
	assert.Equal("alpha-basic", got[0].Models[0].ID)
	assert.Equal("Alpha Basic", got[0].Models[0].Name)
	assert.False(got[0].Models[0].Reasoning)
	assert.True(got[0].Models[0].StructuredOutput)
	require.NotNil(got[0].Models[0].InputCostMicroUSDPerMillionTokens)
	assert.Equal(int64(1), *got[0].Models[0].InputCostMicroUSDPerMillionTokens)
	require.NotNil(got[0].Models[0].OutputCostMicroUSDPerMillionTokens)
	assert.Equal(int64(2_500_000), *got[0].Models[0].OutputCostMicroUSDPerMillionTokens)
	require.Len(got[1].Models, 2)
	assert.Equal("@cf/shape-reasoner", got[1].Models[0].ID)
	assert.Equal(int64(132_001), *got[1].Models[0].InputCostMicroUSDPerMillionTokens)
	assert.Equal(int64(1_254_001), *got[1].Models[0].OutputCostMicroUSDPerMillionTokens)
	assert.True(got[1].Models[0].Reasoning)
	assert.False(got[1].Models[0].StructuredOutput)
	assert.Equal("~shape/latest", got[1].Models[1].ID)
	assert.Equal("Shape Latest Alias", got[1].Models[1].Name)
}

func TestModelsDevFetchRejectsRedirectWithoutFollowing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int32
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "https://models.dev/redirected")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	got, err := client.Fetch(t.Context())
	require.Error(err)
	assert.Nil(got)
	assert.Equal(int32(1), calls.Load())
	assert.NotContains(err.Error(), "/redirected")
}

func TestModelsDevFetchHonorsCallerTimeoutWithSafeError(t *testing.T) {
	release := make(chan struct{})
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	got, err := client.Fetch(ctx)
	close(release)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestNewModelsDevClientDoesNotInheritCallerHTTPConfiguration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	called := &atomic.Bool{}
	callerTransport := &http.Transport{
		Proxy:           func(*http.Request) (*url.URL, error) { return url.Parse("https://user:secret@proxy.invalid") },
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{[]byte(modelsDevRequestCanary)}}}},
	}
	caller := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called.Store(true)
			return nil, errors.New(modelsDevBodyCanary)
		}),
		Jar:           cookieJarWithCanary(t),
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
	}
	client := NewModelsDevClient(caller)
	transport, ok := client.client.Transport.(*http.Transport)
	require.True(ok)
	assert.Nil(transport.Proxy)
	assert.Empty(transport.TLSClientConfig.Certificates)
	assert.Nil(client.client.Jar)
	assert.Equal(modelsDevTotalTimeout, client.client.Timeout)

	caller.Transport = callerTransport
	caller.Jar = cookieJarWithCanary(t)
	callerTransport.TLSClientConfig.Certificates = append(callerTransport.TLSClientConfig.Certificates, tls.Certificate{})
	assert.NotEqual(callerTransport, transport)
	assert.Empty(transport.TLSClientConfig.Certificates)
	assert.False(called.Load())
}

func TestModelsDevFetchRejectsSizeOverflowByOneAndClosesBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	closed := &atomic.Bool{}
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(w, zeroReader{}, modelsDevMaxBodyBytes+1)
	}))
	serverClientTransport := client.client.Transport
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := serverClientTransport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body = &closeTrackingBody{ReadCloser: response.Body, closed: closed}
		return response, nil
	})
	t.Cleanup(server.Close)

	got, err := client.Fetch(t.Context())
	require.Error(err)
	assert.Nil(got)
	assert.True(closed.Load())
	assert.NotContains(err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchDrainsAndClosesStatusErrorWithoutBodyDisclosure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	closed := &atomic.Bool{}
	body := strings.Repeat(modelsDevBodyCanary, 1024)
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, body)
	}))
	base := client.client.Transport
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body = &closeTrackingBody{ReadCloser: response.Body, closed: closed}
		return response, nil
	})
	t.Cleanup(server.Close)

	got, err := client.Fetch(t.Context())
	require.Error(err)
	assert.Nil(got)
	assert.True(closed.Load())
	assert.NotContains(err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchRejectsDuplicateAndUnsafeCatalogData(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "duplicate provider key", body: `{"same":{"id":"same","name":"One","models":{}},"same":{"id":"same","name":"Two","models":{}}}`},
		{name: "duplicate provider id", body: `{"one":{"id":"same","name":"One","models":{}},"two":{"id":"same","name":"Two","models":{}}}`},
		{name: "duplicate model key", body: `{"one":{"id":"one","name":"One","models":{"same":{"id":"same","name":"One"},"same":{"id":"same","name":"Two"}}}}`},
		{name: "duplicate model id", body: `{"one":{"id":"one","name":"One","models":{"first":{"id":"same","name":"One"},"second":{"id":"same","name":"Two"}}}}`},
		{name: "unsafe provider id", body: `{"bad id":{"id":"bad id","name":"Bad","models":{}}}`},
		{name: "unsafe model id", body: `{"one":{"id":"one","name":"One","models":{"bad id":{"id":"bad id","name":"Bad"}}}}`},
		{name: "oversized name", body: `{"one":{"id":"one","name":"` + strings.Repeat("x", 513) + `","models":{}}}`},
		{name: "credentialed URL", body: `{"one":{"id":"one","name":"One","api":"https://user:secret@example.test/v1","models":{}}}`},
		{name: "queried URL", body: `{"one":{"id":"one","name":"One","api":"https://example.test/v1?host=secret","models":{}}}`},
		{name: "remote plaintext URL", body: `{"one":{"id":"one","name":"One","api":"http://example.test/v1","models":{}}}`},
		{name: "unsafe base template suffix", body: `{"one":{"id":"one","name":"One","api":"${BASE_URL}suffix/v1","models":{}}}`},
		{name: "invalid environment", body: `{"one":{"id":"one","name":"One","env":["BAD ENV"],"models":{}}}`},
		{name: "negative price", body: oneModelCatalog(`{"input":-0.1}`)},
		{name: "overflowing price", body: oneModelCatalog(`{"input":9223372036855}`)},
		{name: "invalid price type", body: oneModelCatalog(`{"input":"secret-price"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)

			got, err := client.Fetch(t.Context())
			require.Error(t, err)
			assert.Nil(t, got)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestModelsDevFetchReturnsStableErrorWithoutPartialSuggestions(t *testing.T) {
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, modelsDevBodyCanary)
	}))
	t.Cleanup(server.Close)
	suggestions, err := client.Fetch(t.Context())
	require.ErrorIs(t, err, ErrModelsDevUnavailable)
	assert.Nil(t, suggestions)
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchCancelsDuringTransformationWithoutPartialSuggestions(t *testing.T) {
	body := `{"a":{"id":"a","name":"A","models":{}},"b":{"id":"b","name":"B","models":{}}}`
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	client.hooks = &modelsDevHooks{afterProvider: cancel}

	suggestions, err := client.Fetch(ctx)
	require.ErrorIs(t, err, ErrModelsDevTimeout)
	assert.Nil(t, suggestions)
}

func modelsDevTLSFixture(t *testing.T, handler http.Handler) (*ModelsDevClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	certificate := server.Certificate()
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	target := server.Listener.Addr().String()
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	return newModelsDevClientForTest(dial, pool, certificate.DNSNames[0]), server
}

func cookieJarWithCanary(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	catalogURL, err := url.Parse(modelsDevURL)
	require.NoError(t, err)
	jar.SetCookies(catalogURL, []*http.Cookie{{
		Name: "session", Value: modelsDevRequestCanary, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}})
	return jar
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type closeTrackingBody struct {
	io.ReadCloser

	closed *atomic.Bool
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func oneModelCatalog(cost string) string {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(map[string]any{
		"one": map[string]any{
			"id": "one", "name": "One", "models": map[string]any{
				"model": json.RawMessage(`{"id":"model","name":"Model","cost":` + cost + `}`),
			},
		},
	}); err != nil {
		panic(err)
	}
	return buffer.String()
}

// TestIndependentlyTrustedEndpointAcceptsFirstPartyHostsForOwnProtocol
// pins the compiled-in first-party API hosts that a catalog suggestion may
// pair with a credential: paths and the default port may vary because the
// credential is only ever presented to the first-party TLS endpoint.
func TestIndependentlyTrustedEndpointAcceptsFirstPartyHostsForOwnProtocol(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		endpoint string
	}{
		{"openai chat base", ProtocolOpenAIChat, "https://api.openai.com/v1"},
		{"openai chat trailing slash", ProtocolOpenAIChat, "https://api.openai.com/v1/"},
		{"openai responses base", ProtocolOpenAIResponses, "https://api.openai.com/v1"},
		{"openai default port", ProtocolOpenAIChat, "https://api.openai.com:443/v1"},
		{"openai uppercase host", ProtocolOpenAIChat, "https://API.OpenAI.COM/v1"},
		{"anthropic base", ProtocolAnthropicMessages, "https://api.anthropic.com"},
		{"google versioned path", ProtocolGoogleGenerateContent, "https://generativelanguage.googleapis.com/v1beta"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.True(t, IndependentlyTrustedEndpoint(test.protocol, test.endpoint))
		})
	}
}

// TestIndependentlyTrustedEndpointRejectsEverythingElse ensures a catalog
// cannot redirect a credential to any host, scheme, port, or protocol that
// this binary did not independently ship.
func TestIndependentlyTrustedEndpointRejectsEverythingElse(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		endpoint string
	}{
		{"empty endpoint", ProtocolOpenAIChat, ""},
		{"attacker host", ProtocolOpenAIChat, "https://catalog.example.test/v1"},
		{"lookalike host", ProtocolOpenAIChat, "https://api.openai.com.attacker.test/v1"},
		{"subdomain host", ProtocolOpenAIChat, "https://evil.api.openai.com/v1"},
		{"plain http", ProtocolOpenAIChat, "http://api.openai.com/v1"},
		{"loopback http", ProtocolOpenAIChat, "http://127.0.0.1/v1"},
		{"non-default port", ProtocolOpenAIChat, "https://api.openai.com:8443/v1"},
		{"embedded user info", ProtocolOpenAIChat, "https://key@api.openai.com/v1"},
		{"query string", ProtocolOpenAIChat, "https://api.openai.com/v1?leak=1"},
		{"fragment", ProtocolOpenAIChat, "https://api.openai.com/v1#leak"},
		{"unexpanded template", ProtocolOpenAIChat, "${OPENAI_BASE_URL}/v1"},
		{"cross-vendor host", ProtocolAnthropicMessages, "https://api.openai.com/v1"},
		{"codex has no trusted endpoint", ProtocolCodexAppServer, "https://api.openai.com/v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.False(t, IndependentlyTrustedEndpoint(test.protocol, test.endpoint))
		})
	}
}
