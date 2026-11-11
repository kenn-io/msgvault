package wakeups

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
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

func TestWebhookSigns(t *testing.T) {
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
		category, _ := testWebhookClient(t, server).sendEvent(t.Context(), "https://example.com/hook", "wk_test", "", secret, body)
		assert.Empty(category)
		assert.Equal("wk_test", id)
		assert.NotEmpty(stamp)
		mac := hmac.New(sha256.New, secret)
		_, err := fmt.Fprintf(mac, "%s.%s.%s", id, stamp, body)
		Require.NoError(t, err)
		assert.Equal("v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)), signature)
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
	category, _ := client.sendEvent(t.Context(), "https://example.com/hook", "wk_1", "", []byte(strings.Repeat("a", 32)), []byte("{}"))
	assert.Empty(category)
	assert.Equal([]string{"10.0.0.5:443"}, dialed)
}

type failingResolver struct{}

func (failingResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	return nil, &net.DNSError{Err: "lookup failed", Name: "example.com"}
}

func TestWebhookFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		client     func(*testing.T) webhookClient
	}{
		{"unsafe target", "invalid_callback", func(t *testing.T) webhookClient {
			t.Helper()
			return webhookClient{timeout: time.Second, resolver: staticResolver{netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("127.0.0.1")}}
		}},
		{"dns", "dns_error", func(t *testing.T) webhookClient {
			t.Helper()
			return webhookClient{timeout: time.Second, resolver: failingResolver{}}
		}},
		{"redirect", "http_3xx", func(t *testing.T) webhookClient {
			t.Helper()
			hits := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits++
				w.Header().Set("Location", "https://example.com/redirect")
				w.WriteHeader(http.StatusFound)
			}))
			t.Cleanup(func() { server.Close(); Assert.Equal(t, 1, hits) })
			return testWebhookClient(t, server)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.client(t)
			category, _ := client.sendEvent(t.Context(), "https://example.com/hook", "wk_1", "", []byte(strings.Repeat("a", 32)), []byte("{}"))
			Assert.Equal(t, tc.want, category)
		})
	}
	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			client := webhookClient{timeout: 50 * time.Millisecond, resolver: staticResolver{netip.MustParseAddr("203.0.113.1")}, dial: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }}
			category, _ := client.sendEvent(t.Context(), "https://example.com/hook", "wk_1", "", []byte(strings.Repeat("a", 32)), []byte("{}"))
			Assert.Equal(t, "timeout", category)
		})
	})
}

func TestNativeWebhookSignatureAndSubscriptionHeader(t *testing.T) {
	assert := Assert.New(t)
	secret := []byte(strings.Repeat("s", 32))
	body := []byte(`{"eventId":"evt_0011223344556677","name":"msgvault.messages.changed","timestamp":"2026-01-01T00:00:00Z","data":{"subscription_id":"sub-1"},"cursor":null}`)
	var id, stamp, signature, subscription string
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id = r.Header.Get("webhook-id")
		stamp = r.Header.Get("webhook-timestamp")
		signature = r.Header.Get("webhook-signature")
		subscription = r.Header.Get("X-MCP-Subscription-Id")
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	category, status := testWebhookClient(t, server).sendEvent(t.Context(), "https://example.com/hook", "evt_0011223344556677", "sub-1", secret, body)
	assert.Empty(category)
	assert.Equal(http.StatusNoContent, status)
	assert.Equal("sub-1", subscription)
	assert.Equal(body, received)
	mac := hmac.New(sha256.New, secret)
	_, err := fmt.Fprintf(mac, "%s.%s.%s", id, stamp, received)
	Require.NoError(t, err)
	assert.Equal("v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)), signature)
}
