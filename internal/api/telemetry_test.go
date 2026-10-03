package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/telemetry"
)

const appOpenedBody = `{"event":"app_opened"}`

// countingHandler records how often the router hands a request to the capture handler.
type countingHandler struct {
	next  http.Handler
	calls atomic.Int32
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	h.next.ServeHTTP(w, r)
}

func newTelemetryTestServer(t *testing.T, apiKey string) (*Server, *countingHandler) {
	t.Helper()
	// TestMain opts out: allowlisted events answer "disabled" and nothing is sent.
	reporter := telemetry.NewReporterOrDisabled(telemetry.Options{DataDir: t.TempDir()}, testLogger())
	capture := &countingHandler{next: telemetry.CaptureHandler(reporter)}
	srv := NewServerWithOptions(ServerOptions{
		Config:           &config.Config{Server: config.ServerConfig{APIKey: apiKey}},
		Logger:           testLogger(),
		TelemetryCapture: capture,
	})
	return srv, capture
}

func telemetryRequest(body []byte, contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, telemetryEventsPath, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:4242"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func serveTelemetry(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

// paddedAppOpened returns an app_opened body of exactly size bytes, padded inside the object.
func paddedAppOpened(t *testing.T, size int) []byte {
	t.Helper()
	prefix := `{"event":"app_opened","properties":{"pad":"`
	suffix := `"}}`
	pad := size - len(prefix) - len(suffix)
	require.Positive(t, pad)
	body := []byte(prefix + strings.Repeat("x", pad) + suffix)
	require.Len(t, body, size)
	return body
}

func TestTelemetryEventRouteKeylessLoopback(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	srv, capture := newTelemetryTestServer(t, "")

	accepted := serveTelemetry(srv, telemetryRequest([]byte(appOpenedBody), "application/json"))
	require.Equal(http.StatusAccepted, accepted.Code, accepted.Body.String())
	var status TelemetryEventResponse
	require.NoError(json.Unmarshal(accepted.Body.Bytes(), &status))
	assert.Equal("disabled", status.Status)

	for _, event := range []string{"App_Opened", "daemon_started"} {
		resp := serveTelemetry(srv, telemetryRequest([]byte(`{"event":"`+event+`"}`), "application/json"))
		assert.Equal(http.StatusBadRequest, resp.Code, "event %s: %s", event, resp.Body.String())
	}

	const kitBodyCap = 64 << 10
	atCap := serveTelemetry(srv, telemetryRequest(paddedAppOpened(t, kitBodyCap), "application/json"))
	assert.Equal(http.StatusAccepted, atCap.Code, "body of exactly %d bytes", kitBodyCap)

	rejections := []struct {
		name   string
		body   []byte
		status int
	}{
		{"one byte over the cap", paddedAppOpened(t, kitBodyCap+1), http.StatusRequestEntityTooLarge},
		{"second JSON value", []byte(appOpenedBody + appOpenedBody), http.StatusBadRequest},
		{"trailing junk", []byte(appOpenedBody + "junk"), http.StatusBadRequest},
	}
	for _, tc := range rejections {
		resp := serveTelemetry(srv, telemetryRequest(tc.body, "application/json"))
		assert.Equal(tc.status, resp.Code, "%s: %s", tc.name, resp.Body.String())
	}
	before := capture.calls.Load()
	plain := serveTelemetry(srv, telemetryRequest([]byte(appOpenedBody), "text/plain"))
	assert.Equal(http.StatusUnsupportedMediaType, plain.Code, plain.Body.String())
	assert.Equal(before, capture.calls.Load(), "a non-JSON content type must never reach the capture handler")
}

func TestTelemetryEventRouteAuthentication(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newTelemetryTestServer(t, "owner-key")
	reg := agentgrant.NewRegistry()
	srv.agentGrants = reg

	none := serveTelemetry(srv, telemetryRequest([]byte(appOpenedBody), "application/json"))
	assert.Equal(http.StatusUnauthorized, none.Code, none.Body.String())

	keyed := telemetryRequest([]byte(appOpenedBody), "application/json")
	keyed.Header.Set("X-Api-Key", "owner-key")
	assert.Equal(http.StatusAccepted, serveTelemetry(srv, keyed).Code)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("telemetry-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
	require.NoError(err)
	delegated := telemetryRequest([]byte(appOpenedBody), "application/json")
	delegated.Header.Set(apiprotocol.AgentTokenHeader, secret)
	resp := serveTelemetry(srv, delegated)
	assert.Equal(http.StatusUnauthorized, resp.Code, resp.Body.String())
}

func TestTelemetryEventRouteSessionCSRF(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newTelemetryTestServer(t, "owner-key")
	id, session, err := srv.sessions.create()
	require.NoError(err)

	sessionRequest := func(origin, csrf string) *http.Request {
		req := telemetryRequest([]byte(appOpenedBody), "application/json")
		req.Header.Set("Cookie", sessionCookieName+"="+id)
		req.Header.Set("Origin", origin)
		if csrf != "" {
			req.Header.Set(csrfHeaderName, csrf)
		}
		return req
	}

	ok := serveTelemetry(srv, sessionRequest("http://example.com", session.CSRFToken))
	assert.Equal(http.StatusAccepted, ok.Code, ok.Body.String())

	noToken := serveTelemetry(srv, sessionRequest("http://example.com", ""))
	assert.Equal(http.StatusForbidden, noToken.Code)
	assert.Contains(noToken.Body.String(), "csrf_rejected")

	foreign := serveTelemetry(srv, sessionRequest("http://evil.example", session.CSRFToken))
	assert.Equal(http.StatusForbidden, foreign.Code, foreign.Body.String())
}

func TestTelemetryEventRouteWithoutReporterAdmitsNothing(t *testing.T) {
	t.Parallel()
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{},
		Logger: testLogger(),
	})
	resp := serveTelemetry(srv, telemetryRequest([]byte(appOpenedBody), "application/json"))
	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
}

func TestTelemetryEventRouteBypassesHeldOperationGate(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	require := require.New(t)
	assert := assert.New(t)

	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })

	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(context.Background(), "msgvault sync")
	require.True(ok, "occupy gate")
	defer release()

	reporter := telemetry.NewReporterOrDisabled(telemetry.Options{DataDir: t.TempDir()}, testLogger())
	srv := NewServerWithOptions(ServerOptions{
		Config:           &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:            &gateFilesStore{mockStore: &mockStore{}},
		Logger:           testLogger(),
		OperationGate:    gate,
		TelemetryCapture: telemetry.CaptureHandler(reporter),
	})

	resp := serveTelemetry(srv, telemetryRequest([]byte(appOpenedBody), "application/json"))
	assert.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	assert.False(gate.HasRequestWaiters(), "telemetry must not queue on the gate")

	mutating := httptest.NewRequest(http.MethodPost, "/api/v1/deletions", strings.NewReader(`{}`))
	mutating.Header.Set("Content-Type", "application/json")
	gated := httptest.NewRecorder()
	srv.Router().ServeHTTP(gated, mutating)
	require.Equal(http.StatusServiceUnavailable, gated.Code, "mutating POST must still gate")
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(gated.Body.Bytes(), &errResp))
	assert.Equal("operation_in_progress", errResp.Error)
}
