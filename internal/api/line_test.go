package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
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

func newLineAPIServer(t *testing.T, apiKey string, configure ...func(*Server)) lineAPIServerFixture {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000000")
	require.NoError(t, err)
	require.NoError(t, st.SetLineBotContext(t.Context(), source.ID, lineTestBot))
	channels := []line.ReceiverChannel{{ChannelID: "2000000000", SourceID: source.ID, BotUserID: lineTestBot, Secret: lineTestSecret}}
	wakes := new(atomic.Int64)
	receiver, err := line.NewReceiver(st, channels, func(string) { wakes.Add(1) })
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.BindAddr = "127.0.0.1"
	cfg.Server.APIKey = apiKey
	server := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Logger: testLogger(), LineWebhook: receiver, OperationGate: &recordingOperationGate{allow: false}})
	for _, configureServer := range configure {
		configureServer(server)
	}
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

	fixture := newLineAPIServer(t, "")
	server, st, sourceID, base, wakes := fixture.server, fixture.st, fixture.sourceID, fixture.base, fixture.wakes
	must.True(server.listenerBound)
	body := lineEnvelope("[" + lineEvent("security-event") + "]")
	headers := lineSignedHeaders(body)
	headers.Set("Origin", "https://hostile.example.com")
	headers.Set("Cookie", sessionCookieName+"=ambient-cookie")
	headers.Set("Forwarded", `host="malformed`)
	status, response := lineAPIRequest(t, base, "POST", lineTestPath, body, headers)
	checks.Equal(200, status, response)
	rows, err := st.DueLineEventsContext(context.Background(), sourceID, time.Now(), 10)
	must.NoError(err)
	must.Len(rows, 1)
	checks.Equal(int64(1), wakes.Load())
	for _, tc := range []struct{ method, path string }{
		{"GET", lineTestPath}, {"POST", "/other"},
	} {
		status, response := lineAPIRequest(t, base, tc.method, tc.path, body, lineSignedHeaders(body))
		checks.Equal(403, status, tc.path+": "+response)
		checks.Contains(response, "untrusted_host")
	}
	checks.Equal(int64(1), wakes.Load())
	// Unknown channels also pass the host boundary.
	_, response = lineAPIRequest(t, base, "POST", "/webhooks/line/2000000001", body, lineSignedHeaders(body))
	checks.NotContains(response, "untrusted_host")
}

func TestLineWebhookBurstWithAPIKey(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	fixture := newLineAPIServer(t, "synthetic-api-key")
	for i := range 40 {
		body := lineEnvelope("[" + lineEvent(fmt.Sprintf("burst-%d", i)) + "]")
		status, response := lineAPIRequest(t, fixture.base, "POST", lineTestPath, body, lineSignedHeaders(body))
		checks.Equal(http.StatusOK, status, response)
	}
	rows, err := fixture.st.DueLineEventsContext(t.Context(), fixture.sourceID, time.Now(), 100)
	must.NoError(err)
	checks.Len(rows, 40)
}

func TestLineWebhookCLIHeaderCannotDisableDeadlines(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	fixture := newLineAPIServer(t, "", func(server *Server) {
		server.readTimeout = 250 * time.Millisecond //nolint:kennlint // deadlinetest: unfinished webhook bodies must release ingress without another client write
	})

	request := httptest.NewRequest(http.MethodPost, lineTestPath, nil)
	request.RemoteAddr = "127.0.0.1:4242"
	request.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
	fixture.server.timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, bounded := r.Context().Deadline()
		checks.True(bounded, "webhooks keep a finite context deadline despite the CLI header")
		checks.True(deadline.After(time.Now()))
	})).ServeHTTP(httptest.NewRecorder(), request)

	readers := make([]*bufio.Reader, 0, 16)
	for range 16 {
		conn, err := net.DialTimeout("tcp4", strings.TrimPrefix(fixture.base, "http://"), 5*time.Second)
		must.NoError(err)
		t.Cleanup(func() { _ = conn.Close() })
		must.NoError(conn.SetDeadline(time.Now().Add(5 * time.Second)))
		_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: archive.example.com\r\n%s: %s\r\nContent-Length: 100\r\nExpect: 100-continue\r\n\r\n{", lineTestPath, apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
		must.NoError(err)
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, request)
		must.NoError(err)
		must.Equal(http.StatusContinue, response.StatusCode, "receiver has admitted the unfinished body")
		must.NoError(response.Body.Close())
		readers = append(readers, reader)
	}
	for _, reader := range readers {
		response, err := http.ReadResponse(reader, request)
		must.NoError(err, "unfinished unsigned requests must expire while their connections remain open")
		checks.Equal(http.StatusBadRequest, response.StatusCode)
		must.NoError(response.Body.Close())
	}
	body := lineEnvelope("[" + lineEvent("after-read-deadline") + "]")
	status, response := lineAPIRequest(t, fixture.base, http.MethodPost, lineTestPath, body, lineSignedHeaders(body))
	checks.Equal(http.StatusOK, status, response)
}
