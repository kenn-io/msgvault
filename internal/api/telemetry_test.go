package api

import (
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
	capture := &countingHandler{next: telemetry.CaptureHandler(reporter, t.TempDir())}
	srv := NewServerWithOptions(ServerOptions{
		Config:           &config.Config{Server: config.ServerConfig{APIKey: apiKey}},
		Logger:           testLogger(),
		TelemetryCapture: capture,
	})
	return srv, capture
}

func telemetryRequest(contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, telemetryEventsPath, strings.NewReader(appOpenedBody))
	req.RemoteAddr = "127.0.0.1:4242"
	req.Header.Set("Content-Type", contentType)
	return req
}

func serveTelemetry(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

func TestTelemetryEventRouteKeylessLoopback(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	srv, capture := newTelemetryTestServer(t, "")

	accepted := serveTelemetry(srv, telemetryRequest("application/json"))
	require.Equal(http.StatusAccepted, accepted.Code, accepted.Body.String())
	var status TelemetryEventResponse
	require.NoError(json.Unmarshal(accepted.Body.Bytes(), &status))
	assert.Equal("disabled", status.Status)

	before := capture.calls.Load()
	plain := serveTelemetry(srv, telemetryRequest("text/plain"))
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

	none := serveTelemetry(srv, telemetryRequest("application/json"))
	assert.Equal(http.StatusUnauthorized, none.Code, none.Body.String())

	keyed := telemetryRequest("application/json")
	keyed.Header.Set("X-Api-Key", "owner-key")
	assert.Equal(http.StatusAccepted, serveTelemetry(srv, keyed).Code)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("telemetry-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
	require.NoError(err)
	delegated := telemetryRequest("application/json")
	delegated.Header.Set(apiprotocol.AgentTokenHeader, secret)
	resp := serveTelemetry(srv, delegated)
	assert.Equal(http.StatusUnauthorized, resp.Code, resp.Body.String())
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
		TelemetryCapture: telemetry.CaptureHandler(reporter, t.TempDir()),
	})

	resp := serveTelemetry(srv, telemetryRequest("application/json"))
	assert.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	assert.False(gate.HasRequestWaiters(), "telemetry must not queue on the gate")
}
