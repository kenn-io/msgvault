package mcpevents

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEnabledEventsRejectUnknownSourceTypes(t *testing.T) {
	assert := Assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	opts := Options{Enabled: true, Sources: []string{"gmail", "gmial"}, KeyPath: filepath.Join(t.TempDir(), "key"), OwnerKey: "synthetic-owner"}
	_, err := New(t.Context(), f.Store, opts)
	require.Error(err)
	assert.Contains(err.Error(), `"gmial"`, "the error must name the unknown source type")
	for _, supported := range []string{"gmail", "imap", "gcal", "beeper", "slack", "slackdump", "teams", "discord"} {
		assert.Contains(err.Error(), supported, "the error must list supported source types")
	}
	_, statErr := os.Stat(opts.KeyPath)
	assert.True(os.IsNotExist(statErr), "validation must fail before Events state is created")
	opts.Enabled = false
	_, err = New(t.Context(), f.Store, opts)
	require.NoError(err, "disabled Events ignore source settings")
}

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
