package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dnsRebindRequest builds a request against srv's router for
// /api/v1/settings with the given method and Host header, optionally setting
// an Origin header (empty origin omits the header entirely).
func dnsRebindRequest(srv *Server, method, host, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/settings", nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

// decodeError decodes the ErrorResponse body written by writeError.
func decodeError(t *testing.T, w *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp))
	return errResp
}

// assertNotRejectedAsUntrustedHost verifies a legitimate request was not
// blocked by the DNS-rebinding guard. Any other status (e.g. a downstream
// settings error unrelated to this guard) is acceptable.
func assertNotRejectedAsUntrustedHost(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		return
	}
	assert.NotEqual(t, "untrusted_host", decodeError(t, w).Error, "body: %s", w.Body.String())
}

// TestDNSRebind_AttackerMutationRejected covers the primary attack: a
// rebound Host that matches the forged Origin still fails because the Host
// authority is not loopback.
func TestDNSRebind_AttackerMutationRejected(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	srv.listenerBound = true
	srv.listenPort = 8080

	w := dnsRebindRequest(srv, http.MethodPatch, "evil.com:8080", "http://evil.com:8080")

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "untrusted_host", decodeError(t, w).Error)
}

// TestDNSRebind_AttackerReadRejected covers information disclosure: a GET
// request needs no Origin header at all, so the pre-existing same-origin
// checks (which only fire when Origin is present) would not have caught it.
func TestDNSRebind_AttackerReadRejected(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	srv.listenerBound = true
	srv.listenPort = 8080

	w := dnsRebindRequest(srv, http.MethodGet, "attacker.example:8080", "")

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "untrusted_host", decodeError(t, w).Error)
}

// TestDNSRebind_LegitimateLoopbackAllowed verifies a genuine 127.0.0.1
// request on the bound port is not rejected by the guard.
func TestDNSRebind_LegitimateLoopbackAllowed(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	srv.listenerBound = true
	srv.listenPort = 8080

	w := dnsRebindRequest(srv, http.MethodPatch, "127.0.0.1:8080", "http://127.0.0.1:8080")

	assertNotRejectedAsUntrustedHost(t, w)
}

// TestDNSRebind_LegitimateLocalhostAllowed verifies the "localhost" hostname
// literal on the bound port is not rejected by the guard.
func TestDNSRebind_LegitimateLocalhostAllowed(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	srv.listenerBound = true
	srv.listenPort = 8080

	w := dnsRebindRequest(srv, http.MethodPatch, "localhost:8080", "http://localhost:8080")

	assertNotRejectedAsUntrustedHost(t, w)
}

// TestDNSRebind_WrongPortRejected verifies a loopback hostname on a port
// other than the actual bound listener port is rejected: a port mismatch
// indicates the request did not actually reach the real listener the way it
// claims to (defense-in-depth per review).
func TestDNSRebind_WrongPortRejected(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	srv.listenerBound = true
	srv.listenPort = 8080

	w := dnsRebindRequest(srv, http.MethodPatch, "127.0.0.1:9999", "http://127.0.0.1:9999")

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "untrusted_host", decodeError(t, w).Error)
}

// TestDNSRebind_InertWithoutBoundListener proves the guard stays inert for
// direct-handler unit tests that drive the router without ever binding a
// real listener via StartOnListener (listenerBound defaults to false), so
// the ~345 pre-existing direct-Router tests are unaffected by this change.
func TestDNSRebind_InertWithoutBoundListener(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	w := dnsRebindRequest(srv, http.MethodGet, "evil.com", "")

	assertNotRejectedAsUntrustedHost(t, w)
}

func TestIsLoopbackAuthority(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		listenPort int
		want       bool
	}{
		{"loopback ipv4 matching port", "127.0.0.1:8080", 8080, true},
		{"localhost matching port", "localhost:8080", 8080, true},
		{"loopback ipv6 matching port", "[::1]:8080", 8080, true},
		{"registered hostname", "evil.com:8080", 8080, false},
		{"loopback ipv4 wrong port", "127.0.0.1:9999", 8080, false},
		{"loopback ipv4 no port in host", "127.0.0.1", 8080, true},
		{"non-loopback ipv4", "10.0.0.5:8080", 8080, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isLoopbackAuthority(tc.host, tc.listenPort))
		})
	}
}
