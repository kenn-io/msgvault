package mcpevents

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"

	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"github.com/stretchr/testify/require"
)

func TestOptionsNetworkInjectionPreservesGuardedTLS(t *testing.T) {
	s, f, req := eventService(t)
	w := tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	opts := s.opts
	opts.LookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) { return w.resolve(ctx, "ip", host) }
	opts.DialContext = w.dial
	opts.TLSConfig = w.transport.TLSClientConfig.Clone()
	injected, err := New(t.Context(), f.Store, opts)
	require.NoError(t, err)
	_, err = injected.Subscribe(t.Context(), injected.principal, req)
	require.NoError(t, err)
	opts.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Verify that the constructor rejects insecure injected TLS.
	_, err = New(t.Context(), f.Store, opts)
	require.Error(t, err, "TLS injection cannot disable hostname verification")
}
