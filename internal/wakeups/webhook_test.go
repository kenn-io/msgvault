package wakeups

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
)

func TestWebhookSignsAndDoesNotFollowRedirect(t *testing.T) {
	secret := []byte("12345678901234567890123456789012")
	body := []byte(`{"hello":"world"}`)
	t.Run("signature", func(t *testing.T) {
		assert := Assert.New(t)

		var signature, id, stamp string
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, stamp, signature = r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp"), r.Header.Get("Webhook-Signature")
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()
		assert.Empty(testWebhookClient(t, server).send(t.Context(), "https://example.com/hook", "wk_test", secret, body))
		assert.Equal("wk_test", id)
		assert.NotEmpty(stamp)
		mac := hmac.New(sha256.New, secret)
		_, err := fmt.Fprintf(mac, "%s.%s.%s", id, stamp, body)
		Require.NoError(t, err)
		assert.Equal("v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)), signature)
	})
	t.Run("redirect", func(t *testing.T) {
		hits := 0
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.Header().Set("Location", "https://example.com/redirect")
			w.WriteHeader(http.StatusFound)
		}))
		defer server.Close()
		Assert.Equal(t, "http_3xx", testWebhookClient(t, server).send(t.Context(), "https://example.com/hook", "wk_test", secret, body))
		Assert.Equal(t, 1, hits)
	})
}
func TestDecodeSecretCanonical(t *testing.T) {
	key := []byte(strings.Repeat("a", 32))
	encoded := "whsec_" + base64.StdEncoding.EncodeToString(key)
	got, err := DecodeSecret(encoded)
	Require.NoError(t, err)
	Assert.Equal(t, key, got)
	for _, bad := range []string{"inline", "whsec_YQ==", encoded[:12] + "\n" + encoded[12:]} {
		_, err := DecodeSecret(bad)
		Assert.Error(t, err)
	}
}

type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}
func testWebhookClient(t *testing.T, server *httptest.Server) webhookClient {
	t.Helper()
	transport, ok := server.Client().Transport.(*http.Transport)
	Require.True(t, ok)
	roots := transport.TLSClientConfig.Clone()
	roots.ServerName = "example.com"
	return webhookClient{timeout: time.Second, resolver: staticResolver{netip.MustParseAddr("203.0.113.1")}, roots: roots, dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
}
func TestWebhookMapsUnsafeTarget(t *testing.T) {
	client := webhookClient{timeout: time.Second, resolver: staticResolver{netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("127.0.0.1")}}
	Assert.Equal(t, "invalid_callback", client.send(t.Context(), "https://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
}

func TestWebhookUsesOnlyTrustedPin(t *testing.T) {
	assert := Assert.New(t)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	origin, err := url.Parse("https://example.com")
	Require.NoError(t, err)
	var dialed []string
	client := testWebhookClient(t, server)
	client.resolver = staticResolver{netip.MustParseAddr("127.0.0.1")}
	client.trustedOrigin = origin
	client.trusted = []netip.Addr{netip.MustParseAddr("10.0.0.5")}
	client.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	// The standard httptest certificate includes example.com.
	client.roots = client.roots.Clone()
	client.roots.ServerName = "example.com"
	assert.Empty(client.send(t.Context(), "https://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
	assert.Equal([]string{"10.0.0.5:443"}, dialed)
}
func TestWebhookTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timeout := webhookClient{timeout: 50 * time.Millisecond, resolver: staticResolver{netip.MustParseAddr("203.0.113.1")}, dial: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }}
		Assert.Equal(t, "timeout", timeout.send(t.Context(), "https://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
	})
}

type failingResolver struct{}

func (failingResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	return nil, &net.DNSError{Err: "lookup failed", Name: "example.com"}
}

func TestWebhookDNSFailure(t *testing.T) {
	client := webhookClient{timeout: time.Second, resolver: failingResolver{}}
	Assert.Equal(t, "dns_error", client.send(t.Context(), "https://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
}
