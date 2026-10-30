package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

const lineTestBot = "U00000000000000000000000000000001"
const lineTestSecret = "synthetic-line-secret"
const lineTestPath = "/webhooks/line/2000000000"

type lineAPIServerFixture struct {
	server   *Server
	st       *store.Store
	sourceID int64
	base     string
	wakes    *atomic.Int64
}

func newLineAPIServer(t *testing.T, apiKey string, enabled bool) lineAPIServerFixture {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	require.NoError(t, err)
	var channels []line.ReceiverChannel
	if enabled {
		require.NoError(t, st.SetLineBotContext(t.Context(), source.ID, lineTestBot))
		channels = []line.ReceiverChannel{{ChannelID: "2000000000", SourceID: source.ID, BotUserID: lineTestBot, SecretEnv: "EXAMPLE_LINE_SECRET", Secret: lineTestSecret}}
	}
	wakes := new(atomic.Int64)
	receiver, err := line.NewReceiver(st, channels, func(string) { wakes.Add(1) })
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.BindAddr = "127.0.0.1"
	cfg.Server.APIKey = apiKey
	server := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Logger: testLogger(), LineWebhook: receiver, OperationGate: &recordingOperationGate{allow: false}})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- server.StartOnListener(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, server.WaitStarted(ctx))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, server.Shutdown(ctx))
		require.ErrorIs(t, <-done, http.ErrServerClosed)
	})
	return lineAPIServerFixture{server: server, st: st, sourceID: source.ID, base: "http://" + listener.Addr().String(), wakes: wakes}
}

func lineAPIRequest(t *testing.T, base, method, path, body string, headers http.Header) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, base+path, strings.NewReader(body))
	require.NoError(t, err)
	request.Host = "archive.example.com"
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(raw)
}

func lineSignedHeaders(body string) http.Header {
	return http.Header{"X-Line-Signature": {lineapi.Sign(lineTestSecret, []byte(body))}, "Content-Type": {"application/json"}}
}
func lineEnvelope(events string) string {
	return `{"destination":"` + lineTestBot + `","events":` + events + `}`
}
func lineEvent(id string) string {
	return `{"type":"message","webhookEventId":"` + id + `","timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"123","type":"text","text":"synthetic"}}`
}

func TestLineReceiverRealListenerSecurityBoundary(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	fixture := newLineAPIServer(t, "", true)
	server, st, sourceID, base, wakes := fixture.server, fixture.st, fixture.sourceID, fixture.base, fixture.wakes
	must.True(server.listenerBound)
	body := lineEnvelope("[" + lineEvent("security-event") + "]")
	headers := lineSignedHeaders(body)
	headers.Set("Origin", "https://hostile.example.com")
	headers.Set("Authorization", "Bearer wrong-key")
	headers.Set("Cookie", sessionCookieName+"=ambient-cookie")
	headers.Set("Forwarded", `host="malformed`)
	status, response := lineAPIRequest(t, base, "POST", lineTestPath, body, headers)
	checks.Equal(200, status, response)
	rows, err := st.DueLineEventsContext(context.Background(), sourceID, time.Now(), 10)
	must.NoError(err)
	must.Len(rows, 1)
	checks.Equal(int64(1), wakes.Load())
	headers.Set("X-Line-Signature", "invalid")
	status, _ = lineAPIRequest(t, base, "POST", lineTestPath, body, headers)
	checks.Equal(401, status)
	for _, tc := range []struct{ method, path string }{
		{"GET", lineTestPath}, {"POST", "/other"},
	} {
		status, response := lineAPIRequest(t, base, tc.method, tc.path, body, lineSignedHeaders(body))
		checks.Equal(403, status, tc.path+": "+response)
		checks.Contains(response, "untrusted_host")
	}
	checks.Equal(int64(1), wakes.Load())
	// The same exact route receives a body-free 404 for an unknown channel.
	status, response = lineAPIRequest(t, base, "POST", "/webhooks/line/2000000001", body, lineSignedHeaders(body))
	checks.Equal(404, status)
	checks.Empty(response)
}

func TestLineReceiverRetainsAPIKeyAndCSRFProtections(t *testing.T) {
	checks := assert.New(t)

	fixture := newLineAPIServer(t, "synthetic-api-key", true)
	server, base := fixture.server, fixture.base
	status, _ := lineAPIRequest(t, base, "GET", "/api/v1/stats", "", nil)
	checks.Equal(401, status)
	status, _ = lineAPIRequest(t, base, "GET", "/api/v1/stats", "", http.Header{"Authorization": {"Bearer synthetic-api-key"}})
	checks.Equal(200, status)
	sessionID, _, err := server.sessions.create()
	require.NoError(t, err)
	headers := http.Header{"Cookie": {sessionCookieName + "=" + sessionID}, "Origin": {"http://archive.example.com"}, "Content-Type": {"application/json"}}
	status, response := lineAPIRequest(t, base, "POST", "/api/v1/query", `{"sql":"SELECT 1"}`, headers)
	checks.Equal(403, status, response)
	checks.Contains(response, "csrf")
	body := lineEnvelope("[]")
	headers.Set("Origin", "https://hostile.example.com")
	headers.Set("Authorization", "Bearer wrong-key")
	headers.Set("X-Line-Signature", lineapi.Sign(lineTestSecret, []byte(body)))
	status, response = lineAPIRequest(t, base, "POST", lineTestPath, body, headers)
	checks.Equal(200, status, response)
	status, response = lineAPIRequest(t, base, "GET", "/api/v1/stats", "", http.Header{"Cookie": {sessionCookieName + "=" + sessionID}, "Origin": {"https://hostile.example.com"}})
	checks.Equal(403, status, response)
	checks.Contains(response, "cross_origin_session")
}
