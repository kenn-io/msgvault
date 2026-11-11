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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookSignsAndDoesNotFollowRedirect(t *testing.T) {
	secret := []byte("12345678901234567890123456789012")
	var signature, id, stamp string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id = r.Header.Get("webhook-id")
		stamp = r.Header.Get("webhook-timestamp")
		signature = r.Header.Get("webhook-signature")
		w.Header().Set("Location", "https://example.com/")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	client := webhookClient{client: server.Client(), timeout: time.Second}
	body := []byte(`{"hello":"world"}`)
	category := client.send(t.Context(), server.URL, "wk_test", secret, body)
	assert.NotEmpty(t, category)
	mac := hmac.New(sha256.New, secret)
	_, err := fmt.Fprintf(mac, "%s.%s.%s", id, stamp, body)
	require.NoError(t, err)
	assert.Equal(t, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)), signature)
}
func TestDecodeSecretCanonical(t *testing.T) {
	key := []byte(strings.Repeat("a", 32))
	encoded := "whsec_" + base64.StdEncoding.EncodeToString(key)
	got, err := DecodeSecret(encoded)
	require.NoError(t, err)
	assert.Equal(t, key, got)
	for _, bad := range []string{"inline", "whsec_YQ==", strings.TrimRight(encoded, "=")} {
		_, err := DecodeSecret(bad)
		assert.Error(t, err)
	}
}

type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}
func TestWebhookRefusesMixedDNSWithoutDialing(t *testing.T) {
	dialed := false
	client := webhookClient{timeout: time.Second, resolver: staticResolver{netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("127.0.0.1")}, dial: func(context.Context, string, string) (net.Conn, error) { dialed = true; return nil, context.Canceled }}
	assert.Equal(t, "invalid_callback", client.send(t.Context(), "https://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
	assert.False(t, dialed)
	assert.Equal(t, "invalid_callback", client.send(t.Context(), "http://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
}

func TestWebhookUsesOnlyTrustedPinAndTimesOut(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	origin, err := url.Parse("https://example.com")
	require.NoError(t, err)
	var dialed []string
	client := webhookClient{timeout: time.Second, resolver: staticResolver{netip.MustParseAddr("127.0.0.1")}, trustedOrigin: origin, trusted: []netip.Addr{netip.MustParseAddr("10.0.0.5")}, roots: server.Client().Transport.(*http.Transport).TLSClientConfig, dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
	// The standard httptest certificate includes example.com.
	client.roots = client.roots.Clone()
	client.roots.ServerName = "example.com"
	assert.Empty(t, client.send(t.Context(), "https://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
	assert.Equal(t, []string{"10.0.0.5:443"}, dialed)
	timeout := webhookClient{timeout: 50 * time.Millisecond, resolver: staticResolver{netip.MustParseAddr("203.0.113.1")}, dial: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }}
	assert.Equal(t, "timeout", timeout.send(t.Context(), "https://example.com/hook", "wk_1", []byte(strings.Repeat("a", 32)), []byte("{}")))
}
