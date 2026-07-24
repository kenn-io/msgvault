package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postLoopbackJSONMutation issues a keyless (loopback-mode) POST with
// attacker- or client-controlled Origin and Content-Type headers, modeling
// both browser-forged and legitimate CLI/localhost requests.
func postLoopbackJSONMutation(
	t *testing.T, srv *Server, path, body, origin, contentType string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func identityLinkBody(a, b int64) string {
	return fmt.Sprintf(`{"participant_a":%d,"participant_b":%d}`, a, b)
}

// requireNoIdentityLink asserts the forged or malformed request left the
// participant link graph untouched: a and b remain in separate clusters and
// no post-mutation cache refresh ran.
func requireNoIdentityLink(t *testing.T, wrapped *stubIdentityCacheStore, a, b int64) {
	t.Helper()
	members, err := wrapped.ClusterMembers(a)
	require.NoError(t, err)
	assert.NotContains(t, members, b, "rejected request must not create a link")
	assert.Equal(t, 0, wrapped.refreshCalls, "no mutation committed, so no cache refresh")
}

// TestLoopbackMutation_CrossOriginTextPlainForgeryRejected covers the forgery
// this change closes: with no API key configured (keyless loopback mode), a
// malicious page visited by the daemon operator can fire a CORS-safelisted
// text/plain POST at http://127.0.0.1:<port> without any preflight. The
// browser attaches the attacker page's Origin header; the daemon must refuse
// the mutation and leave the link graph untouched.
func TestLoopbackMutation_CrossOriginTextPlainForgeryRejected(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "bob@example.com", "Bob", "example.com")

	w := postLoopbackJSONMutation(t, srv, "/api/v1/identity/links",
		identityLinkBody(a, b), "https://evil.example", "text/plain")

	// The same-origin gate in requestSecurityMiddleware fires before the
	// media-type gate, so the cross-origin forgery is rejected with 403.
	assert.Equal(http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal("cross_origin_loopback", errResp.Error)
	requireNoIdentityLink(t, wrapped, a, b)
}

// TestLoopbackMutation_CrossOriginJSONRejected isolates the Origin gate: even
// a well-formed application/json mutation must be refused when the browser
// discloses a cross-site Origin, because keyless loopback access is an
// ambient credential just like a session cookie.
func TestLoopbackMutation_CrossOriginJSONRejected(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "bob@example.com", "Bob", "example.com")

	w := postLoopbackJSONMutation(t, srv, "/api/v1/identity/links",
		identityLinkBody(a, b), "https://evil.example", "application/json")

	assert.Equal(http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal("cross_origin_loopback", errResp.Error)
	requireNoIdentityLink(t, wrapped, a, b)
}

// TestLoopbackMutation_NoOriginJSONSucceeds keeps the loopback use case
// working: curl, the TUI, and other non-browser clients send no Origin
// header and must not be affected by the same-origin gate.
func TestLoopbackMutation_NoOriginJSONSucceeds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "bob@example.com", "Bob", "example.com")

	w := postLoopbackJSONMutation(t, srv, "/api/v1/identity/links",
		identityLinkBody(a, b), "", "application/json")

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	members, err := wrapped.ClusterMembers(a)
	require.NoError(err)
	assert.Contains(members, b)
	assert.Equal(1, wrapped.refreshCalls)
}

// TestLoopbackMutation_SameOriginJSONSucceeds keeps the first-party web UI
// working in keyless mode: a browser fetch from the daemon's own origin
// discloses a matching Origin header and must pass.
func TestLoopbackMutation_SameOriginJSONSucceeds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "bob@example.com", "Bob", "example.com")

	// httptest.NewRequest targets http://example.com, so this Origin is
	// exactly the request's own scheme and host.
	w := postLoopbackJSONMutation(t, srv, "/api/v1/identity/links",
		identityLinkBody(a, b), "http://example.com", "application/json")

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	members, err := wrapped.ClusterMembers(a)
	require.NoError(err)
	assert.Contains(members, b)
}

// TestLoopbackMutation_TextPlainNoOriginRejected415 isolates the media-type
// gate: a mutation body that does not declare application/json is refused
// with 415 even when no Origin header implicates a browser.
func TestLoopbackMutation_TextPlainNoOriginRejected415(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "bob@example.com", "Bob", "example.com")

	w := postLoopbackJSONMutation(t, srv, "/api/v1/identity/links",
		identityLinkBody(a, b), "", "text/plain")

	assert.Equal(http.StatusUnsupportedMediaType, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal("unsupported_media_type", errResp.Error)
	requireNoIdentityLink(t, wrapped, a, b)
}

// TestLoopbackMutation_JSONWithCharsetParameterAccepted allows media-type
// parameters: "application/json; charset=utf-8" is what fetch and many HTTP
// clients actually send.
func TestLoopbackMutation_JSONWithCharsetParameterAccepted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "bob@example.com", "Bob", "example.com")

	w := postLoopbackJSONMutation(t, srv, "/api/v1/identity/links",
		identityLinkBody(a, b), "", "application/json; charset=utf-8")

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	members, err := wrapped.ClusterMembers(a)
	require.NoError(err)
	assert.Contains(members, b)
}

// TestLoopbackMutation_TrailingJSONRejected rejects request smuggling through
// concatenated JSON values: only the first value would be decoded, so a body
// carrying a second value must be refused outright.
func TestLoopbackMutation_TrailingJSONRejected(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, wrapped := newIdentityLinkTestServer(t)
	a := wrapped.mustParticipant(t, "alice@example.com", "Alice", "example.com")
	b := wrapped.mustParticipant(t, "bob@example.com", "Bob", "example.com")

	body := identityLinkBody(a, b) + identityLinkBody(b, a)
	w := postLoopbackJSONMutation(t, srv, "/api/v1/identity/links",
		body, "", "application/json")

	assert.Equal(http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal("invalid_request", errResp.Error)
	requireNoIdentityLink(t, wrapped, a, b)
}

// TestLoopbackMutation_MediaTypeGateCoversOtherJSONRoutes proves the 415 gate
// is wired through the shared route registration, not just identity links.
func TestLoopbackMutation_MediaTypeGateCoversOtherJSONRoutes(t *testing.T) {
	srv, _ := newIdentityLinkTestServer(t)
	for path, body := range map[string]string{
		"/api/v1/query":    `{"sql":"SELECT 1"}`,
		"/api/v1/explore":  `{}`,
		"/api/v1/accounts": `{"email":"user@example.com"}`,
	} {
		t.Run(path, func(t *testing.T) {
			w := postLoopbackJSONMutation(t, srv, path, body, "", "text/plain")
			assert.Equal(t, http.StatusUnsupportedMediaType, w.Code, "body: %s", w.Body.String())
		})
	}
}
