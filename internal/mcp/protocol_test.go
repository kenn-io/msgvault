package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

const task3ModernProtocolVersion = "2026-07-28"

const task5ExpectedServerVersion = "1.0.0"

type task3RPCError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

type task3RPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Result  map[string]any `json:"result"`
	Error   *task3RPCError `json:"error"`
}

func task3ModernRequest(method, name, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", task3ModernProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	return req
}

func task3Serve(handler http.Handler, req *http.Request) (*httptest.ResponseRecorder, task3RPCResponse) {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	var response task3RPCResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	return recorder, response
}

func task3RequireSuccess(t *testing.T, recorder *httptest.ResponseRecorder, response task3RPCResponse) {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code, "response: %s", recorder.Body.String())
	require.Nil(t, response.Error, "response: %s", recorder.Body.String())
}

func task3ToolCallBody(id int, name, arguments string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		id,
		name,
		arguments,
	)
}

func task3ToolErrorText(t *testing.T, response task3RPCResponse) string {
	t.Helper()
	content, ok := response.Result["content"].([]any)
	require.True(t, ok, "result: %#v", response.Result)
	require.NotEmpty(t, content, "result: %#v", response.Result)
	text, ok := content[0].(map[string]any)
	require.True(t, ok, "content: %#v", content)
	value, ok := text["text"].(string)
	require.True(t, ok, "content: %#v", content)
	return value
}

type task5RawStdioPeer struct {
	requestWriter  *os.File
	responseReader *os.File
	responseLines  *bufio.Scanner
	cancel         context.CancelFunc
	done           chan error
}

func newTask5RawStdioPeer(t *testing.T, opts ServeOptions) *task5RawStdioPeer {
	t.Helper()
	return newTask5RawStdioPeerWithServer(t, newMCPServer(opts, true))
}

func newTask5RawStdioPeerWithServer(t *testing.T, server *sdkmcp.Server) *task5RawStdioPeer {
	t.Helper()

	serverRequestReader, clientRequestWriter, err := os.Pipe()
	require.NoError(t, err)
	clientResponseReader, serverResponseWriter, err := os.Pipe()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, &sdkmcp.IOTransport{
			Reader: serverRequestReader,
			Writer: serverResponseWriter,
		})
	}()

	peer := &task5RawStdioPeer{
		requestWriter:  clientRequestWriter,
		responseReader: clientResponseReader,
		responseLines:  bufio.NewScanner(clientResponseReader),
		cancel:         cancel,
		done:           done,
	}
	t.Cleanup(func() {
		cancel()
		_ = clientRequestWriter.Close()
		_ = clientResponseReader.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			assert.Fail(t, "raw stdio server did not stop within timeout")
		}
	})
	return peer
}

func (p *task5RawStdioPeer) writeLiteralLine(t *testing.T, line string) {
	t.Helper()
	require.NoError(t, p.requestWriter.SetWriteDeadline(time.Now().Add(5*time.Second)))
	_, err := p.requestWriter.WriteString(line + "\n")
	require.NoError(t, err)
}

func (p *task5RawStdioPeer) call(t *testing.T, line string) task3RPCResponse {
	t.Helper()
	response, _ := p.callRaw(t, line)
	return response
}

func (p *task5RawStdioPeer) callRaw(t *testing.T, line string) (task3RPCResponse, string) {
	t.Helper()
	p.writeLiteralLine(t, line)
	require.NoError(t, p.responseReader.SetReadDeadline(time.Now().Add(5*time.Second)))
	require.True(t, p.responseLines.Scan(), "raw stdio response: %v", p.responseLines.Err())
	raw := p.responseLines.Text()
	var response task3RPCResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &response), "response: %s", raw)
	return response, raw
}

const task6PostHandlerFailureTool = "post_handler_output_failure"

func task6PostHandlerFailureServer(sentinel string) *sdkmcp.Server {
	server := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}}, true)
	definition := readDefinition(
		task6PostHandlerFailureTool,
		"Return a test result that exercises SDK output validation.",
		closedObject(map[string]*jsonschema.Schema{}),
		closedObject(map[string]*jsonschema.Schema{
			"secret": {Type: "string", Pattern: "^public$"},
		}, "secret"),
		func(_ *handlers, _ context.Context, _ toolRequest) (*toolResult, error) {
			return jsonResult(map[string]any{"secret": sentinel})
		},
	)
	sdkmcp.AddTool[map[string]any, any](
		server,
		definition.tool(),
		officialToolHandler(definition.bind(&handlers{})),
	)
	return server
}

func task6CaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previousLogger := slog.Default()
	logs := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	return logs
}

func task6AssertPostHandlerFailureIsPrivate(
	t *testing.T,
	response task3RPCResponse,
	raw, logs, sentinel string,
) {
	t.Helper()
	require.NotNil(t, response.Error, "response: %s", raw)
	assert.Equal(t, int64(-32603), response.Error.Code, "response: %s", raw)
	assert.Equal(t, "internal server error", response.Error.Message, "response: %s", raw)
	assert.NotContains(t, raw, sentinel)
	assert.Contains(t, logs, sentinel)
}

func TestMCPPostHandlerErrorIsolationModernHTTP(t *testing.T) {
	const sentinel = "task6-http-private-output"
	logs := task6CaptureLogs(t)
	handler := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server {
			return task6PostHandlerFailureServer(sentinel)
		},
		&sdkmcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			PropagateRequestCancellation: true,
			MaxRequestBodyBytes:          1 << 20,
		},
	)
	body := task3ToolCallBody(1, task6PostHandlerFailureTool, `{}`)
	recorder, response := task3Serve(
		handler,
		task3ModernRequest("tools/call", task6PostHandlerFailureTool, body),
	)

	task6AssertPostHandlerFailureIsPrivate(t, response, recorder.Body.String(), logs.String(), sentinel)
}

func TestMCPPostHandlerErrorIsolationModernStdio(t *testing.T) {
	const sentinel = "task6-stdio-private-output"
	logs := task6CaptureLogs(t)
	peer := newTask5RawStdioPeerWithServer(t, task6PostHandlerFailureServer(sentinel))
	response, raw := peer.callRaw(
		t,
		task3ToolCallBody(1, task6PostHandlerFailureTool, `{}`),
	)

	task6AssertPostHandlerFailureIsPrivate(t, response, raw, logs.String(), sentinel)
}

func task5AssertRawJSONParity(t *testing.T, result map[string]any) {
	t.Helper()
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	structured, ok := result["structuredContent"]
	require.True(t, ok, "result: %#v", result)
	require.NotNil(t, structured)
	content, ok := result["content"].([]any)
	require.True(t, ok, "result: %#v", result)
	require.NotEmpty(t, content)
	textContent, ok := content[0].(map[string]any)
	require.True(t, ok, "content: %#v", content)
	text, ok := textContent["text"].(string)
	require.True(t, ok, "content: %#v", textContent)
	require.NotEmpty(t, text)
	var textJSON any
	require.NoError(t, json.Unmarshal([]byte(text), &textJSON))
	assert.Equal(t, textJSON, structured)
}

func TestRawStdioModern(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	fixture := newTask5Fixture(t, "000")
	peer := newTask5RawStdioPeer(t, fixture.opts)

	discover := peer.call(t, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"raw-modern","version":"raw-modern-version"}}}}`)
	must.Nil(discover.Error)
	checks.Equal("complete", discover.Result["resultType"])
	meta, ok := discover.Result["_meta"].(map[string]any)
	must.True(ok, "discover result: %#v", discover.Result)
	checks.Equal(map[string]any{"name": "msgvault", "version": task5ExpectedServerVersion}, meta["io.modelcontextprotocol/serverInfo"])

	listed := peer.call(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	must.Nil(listed.Error)
	checks.Equal(task5ExpectedTools("000", true), task3ToolNames(t, listed))

	called := peer.call(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_stats","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	must.Nil(called.Error)
	checks.Equal("complete", called.Result["resultType"])
	callMeta, ok := called.Result["_meta"].(map[string]any)
	must.True(ok, "tool result: %#v", called.Result)
	checks.Equal(map[string]any{"name": "msgvault", "version": task5ExpectedServerVersion}, callMeta["io.modelcontextprotocol/serverInfo"])
	task5AssertRawJSONParity(t, called.Result)

	removed := peer.call(t, `{"jsonrpc":"2.0","id":4,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	must.NotNil(removed.Error)
	checks.Equal(int64(-32601), removed.Error.Code)
}

func TestRawStdioLegacy(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	fixture := newTask5Fixture(t, "000")
	peer := newTask5RawStdioPeer(t, fixture.opts)

	initialized := peer.call(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw-legacy","version":"raw-legacy-version"}}}`)
	must.Nil(initialized.Error)
	checks.Equal("2025-11-25", initialized.Result["protocolVersion"])
	checks.Equal(map[string]any{"name": "msgvault", "version": task5ExpectedServerVersion}, initialized.Result["serverInfo"])

	peer.writeLiteralLine(t, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	listed := peer.call(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	must.Nil(listed.Error)
	checks.Equal(task5ExpectedTools("000", true), task3ToolNames(t, listed))

	called := peer.call(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_stats","arguments":{}}}`)
	must.Nil(called.Error)
	task5AssertRawJSONParity(t, called.Result)
}

func task5LegacyHTTPPost(
	t *testing.T,
	handler http.Handler,
	protocolVersion string,
	body string,
) (*httptest.ResponseRecorder, task3RPCResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if protocolVersion != "" {
		req.Header.Set("Mcp-Protocol-Version", protocolVersion)
	}
	assert.Equal(t, protocolVersion, req.Header.Get("Mcp-Protocol-Version"))
	assert.Empty(t, req.Header.Get("Mcp-Session-Id"))
	recorder, response := task3Serve(handler, req)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	assert.Empty(t, recorder.Header().Get("Mcp-Session-Id"))
	return recorder, response
}

func TestRawHTTPLegacy(t *testing.T) {
	fixture := newTask5Fixture(t, "000")
	handler := newMCPHTTPServer(fixture.opts, HTTPOptions{}).Handler

	initializeRecorder, initialized := task5LegacyHTTPPost(t, handler, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw-http-legacy","version":"raw-http-legacy-version"}}}`)
	task3RequireSuccess(t, initializeRecorder, initialized)
	assert.Equal(t, "2025-11-25", initialized.Result["protocolVersion"])

	listRecorder, listed := task5LegacyHTTPPost(t, handler, "2025-11-25", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	task3RequireSuccess(t, listRecorder, listed)
	assert.Equal(t, task5ExpectedTools("000", false), task3ToolNames(t, listed))

	callRecorder, called := task5LegacyHTTPPost(t, handler, "2025-11-25", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_stats","arguments":{}}}`)
	task3RequireSuccess(t, callRecorder, called)
	task5AssertRawJSONParity(t, called.Result)
}

func task3ToolNames(t *testing.T, response task3RPCResponse) []string {
	t.Helper()
	tools, ok := response.Result["tools"].([]any)
	require.True(t, ok, "result: %#v", response.Result)
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		require.True(t, ok, "tool: %#v", raw)
		name, ok := tool["name"].(string)
		require.True(t, ok, "tool: %#v", tool)
		names = append(names, name)
	}
	return names
}

func TestMCPModernHTTPDiscovery(t *testing.T) {
	assert := assert.New(t)

	handler := newMCPHTTPServer(ServeOptions{
		Engine: &querytest.MockEngine{},
	}, HTTPOptions{}).Handler
	body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

	recorder, response := task3Serve(handler, task3ModernRequest("server/discover", "", body))

	task3RequireSuccess(t, recorder, response)
	assert.Equal("2.0", response.JSONRPC)
	assert.Equal(1, response.ID)
	assert.Equal("complete", response.Result["resultType"])
	assert.Equal(map[string]any{
		"resources": map[string]any{},
		"tools":     map[string]any{},
	}, response.Result["capabilities"])
	assert.InDelta(float64(3_600_000), response.Result["ttlMs"], 0)
	assert.Equal("public", response.Result["cacheScope"])
	assert.Contains(response.Result["supportedVersions"], task3ModernProtocolVersion)
	meta, ok := response.Result["_meta"].(map[string]any)
	require.True(t, ok, "result: %#v", response.Result)
	assert.Equal(map[string]any{
		"name":    "msgvault",
		"version": "1.0.0",
	}, meta["io.modelcontextprotocol/serverInfo"])
	assert.Empty(recorder.Header().Get("Mcp-Session-Id"))
	assert.Equal("no-store", recorder.Header().Get("Cache-Control"))
}

func TestMCPModernHTTPMetadataValidation(t *testing.T) {
	handler := newMCPHTTPServer(ServeOptions{
		Engine: &querytest.MockEngine{},
	}, HTTPOptions{}).Handler
	tests := []struct {
		name string
		body string
	}{
		{
			name: "missing protocol version",
			body: `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}`,
		},
		{
			name: "missing client capabilities",
			body: `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			recorder, response := task3Serve(
				handler,
				task3ModernRequest("server/discover", "", test.body),
			)

			assert.Equal(http.StatusBadRequest, recorder.Code, "response: %s", recorder.Body.String())
			require.NotNil(t, response.Error, "response: %s", recorder.Body.String())
			assert.Equal(int64(-32602), response.Error.Code)
			assert.Equal("no-store", recorder.Header().Get("Cache-Control"))
		})
	}
}

func TestMCPModernHTTPHeaderAgreement(t *testing.T) {
	handler := newMCPHTTPServer(ServeOptions{
		Engine: &querytest.MockEngine{},
	}, HTTPOptions{}).Handler
	discoverBody := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	toolBody := task3ToolCallBody(1, ToolGetStats, `{}`)
	tests := []struct {
		name       string
		request    *http.Request
		changeHead func(http.Header)
	}{
		{
			name:    "method mismatch",
			request: task3ModernRequest("tools/list", "", discoverBody),
		},
		{
			name:    "name mismatch",
			request: task3ModernRequest("tools/call", ToolStageDeletion, toolBody),
		},
		{
			name:    "version mismatch",
			request: task3ModernRequest("server/discover", "", discoverBody),
			changeHead: func(header http.Header) {
				header.Set("Mcp-Protocol-Version", "2026-07-29")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			if test.changeHead != nil {
				test.changeHead(test.request.Header)
			}
			recorder, response := task3Serve(handler, test.request)

			assert.Equal(http.StatusBadRequest, recorder.Code, "response: %s", recorder.Body.String())
			require.NotNil(t, response.Error, "response: %s", recorder.Body.String())
			assert.Equal(int64(-32020), response.Error.Code)
			assert.Equal("no-store", recorder.Header().Get("Cache-Control"))
		})
	}
}

func TestMCPModernHTTPMethods(t *testing.T) {
	assert := assert.New(t)

	handler := newMCPHTTPServer(ServeOptions{
		Engine: &querytest.MockEngine{},
	}, HTTPOptions{}).Handler
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/mcp", nil)
			recorder, _ := task3Serve(handler, req)

			assert.Equal(http.StatusMethodNotAllowed, recorder.Code)
			assert.Equal(http.MethodPost, recorder.Header().Get("Allow"))
			assert.Equal("no-store", recorder.Header().Get("Cache-Control"))
		})
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	recorder, response := task3Serve(handler, task3ModernRequest("ping", "", body))
	assert.Equal(http.StatusNotFound, recorder.Code, "response: %s", recorder.Body.String())
	require.NotNil(t, response.Error, "response: %s", recorder.Body.String())
	assert.Equal(int64(-32601), response.Error.Code)
	assert.Equal("no-store", recorder.Header().Get("Cache-Control"))
}

func TestMCPModernHTTPOriginAuthAndBodyLimit(t *testing.T) {
	checks := assert.New(t)

	handler := newMCPHTTPServer(ServeOptions{
		Engine: &querytest.MockEngine{},
	}, HTTPOptions{APIKey: "test-api-key"}).Handler
	discoverBody := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

	crossOrigin := task3ModernRequest("server/discover", "", discoverBody)
	crossOrigin.Header.Set("Authorization", "Bearer test-api-key")
	crossOrigin.Header.Set("Origin", "https://attacker.example")
	crossOrigin.Header.Set("Sec-Fetch-Site", "cross-site")
	recorder, _ := task3Serve(handler, crossOrigin)
	checks.Equal(http.StatusForbidden, recorder.Code)
	checks.Empty(recorder.Header().Get("WWW-Authenticate"))
	checks.Equal("no-store", recorder.Header().Get("Cache-Control"))

	for _, test := range []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{name: "missing bearer", wantStatus: http.StatusUnauthorized},
		{name: "wrong bearer", authorization: "Bearer wrong-key", wantStatus: http.StatusUnauthorized},
		{name: "valid bearer", authorization: "Bearer test-api-key", wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			req := task3ModernRequest("server/discover", "", discoverBody)
			if test.authorization != "" {
				req.Header.Set("Authorization", test.authorization)
			}
			recorder, _ := task3Serve(handler, req)
			assert.Equal(test.wantStatus, recorder.Code, "response: %s", recorder.Body.String())
			assert.Equal("no-store", recorder.Header().Get("Cache-Control"))
			if test.wantStatus == http.StatusUnauthorized {
				assert.Equal("Bearer", recorder.Header().Get("WWW-Authenticate"))
			}
		})
	}

	oversized := task3ModernRequest(
		"server/discover",
		"",
		strings.Repeat("x", (1<<20)+1),
	)
	oversized.Header.Set("Authorization", "Bearer test-api-key")
	recorder, _ = task3Serve(handler, oversized)
	checks.Equal(http.StatusRequestEntityTooLarge, recorder.Code, "response: %s", recorder.Body.String())
	checks.Equal("no-store", recorder.Header().Get("Cache-Control"))
}

func TestMCPModernHTTPCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	canceled := make(chan struct{}, 1)
	engine := &querytest.MockEngine{
		GetTotalStatsFunc: func(ctx context.Context, _ query.StatsOptions) (*query.TotalStats, error) {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				canceled <- struct{}{}
				return nil, ctx.Err()
			case <-release:
				return &query.TotalStats{}, nil
			}
		},
	}
	handler := newMCPHTTPServer(ServeOptions{Engine: engine}, HTTPOptions{}).Handler
	ctx, cancel := context.WithCancel(context.Background())
	req := task3ModernRequest("tools/call", ToolGetStats, task3ToolCallBody(1, ToolGetStats, `{}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = task3Serve(handler, req)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		require.FailNow(t, "blocking tool handler did not start")
	}
	cancel()

	wasCanceled := false
	select {
	case <-canceled:
		wasCanceled = true
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "HTTP request did not finish after releasing handler")
	}
	assert.True(t, wasCanceled, "canceling the HTTP request must cancel the real tool handler")
}

type task3CountingManifestSaver struct {
	calls atomic.Int32
}

func (s *task3CountingManifestSaver) SaveManifest(context.Context, *deletion.Manifest) error {
	s.calls.Add(1)
	return nil
}

func TestMCPHTTPPolicyWriteTools(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	destination := t.TempDir()
	saver := &task3CountingManifestSaver{}
	var attachmentReads atomic.Int32
	engine := &querytest.MockEngine{
		Attachments: map[int64]*query.AttachmentInfo{
			1: {
				ID:          1,
				Filename:    "report.txt",
				Size:        7,
				ContentHash: "content-hash",
			},
		},
		SearchFastResults: []query.MessageSummary{{SourceMessageID: "gmail-1"}},
	}
	opts := ServeOptions{
		Engine: engine,
		AttachmentReader: attachmentReaderFunc(func(context.Context, string) ([]byte, error) {
			attachmentReads.Add(1)
			return []byte("content"), nil
		}),
		ManifestSaver: saver,
	}

	listBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	readOnlyHandler := newMCPHTTPServer(opts, HTTPOptions{}).Handler
	recorder, response := task3Serve(readOnlyHandler, task3ModernRequest("tools/list", "", listBody))
	task3RequireSuccess(t, recorder, response)
	names := task3ToolNames(t, response)
	assert.NotContains(names, ToolExportAttachment)
	assert.NotContains(names, ToolStageDeletion)

	writableHandler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	recorder, response = task3Serve(writableHandler, task3ModernRequest("tools/list", "", listBody))
	task3RequireSuccess(t, recorder, response)
	names = task3ToolNames(t, response)
	assert.Contains(names, ToolExportAttachment)
	assert.Contains(names, ToolStageDeletion)

	exportBody := task3ToolCallBody(
		2,
		ToolExportAttachment,
		`{"attachment_id":1,"destination":`+strconv.Quote(destination)+`}`,
	)
	recorder, response = task3Serve(
		readOnlyHandler,
		task3ModernRequest("tools/call", ToolExportAttachment, exportBody),
	)
	assert.Equal(http.StatusBadRequest, recorder.Code, "response: %s", recorder.Body.String())
	require.NotNil(response.Error, "response: %s", recorder.Body.String())

	stageBody := task3ToolCallBody(3, ToolStageDeletion, `{"query":"match"}`)
	recorder, response = task3Serve(
		readOnlyHandler,
		task3ModernRequest("tools/call", ToolStageDeletion, stageBody),
	)
	assert.Equal(http.StatusBadRequest, recorder.Code, "response: %s", recorder.Body.String())
	require.NotNil(response.Error, "response: %s", recorder.Body.String())

	entries, err := os.ReadDir(destination)
	require.NoError(err)
	assert.Empty(entries, "hidden export call must not create a file")
	assert.Equal(int32(0), attachmentReads.Load(), "hidden export call must not read attachment content")
	assert.Equal(int32(0), saver.calls.Load(), "hidden stage call must not save a manifest")
}

func TestMCPInvocationLimitRateBurst(t *testing.T) {
	assert := assert.New(t)

	var calls atomic.Int32
	engine := &querytest.MockEngine{
		Stats: &query.TotalStats{},
		GetTotalStatsFunc: func(context.Context, query.StatsOptions) (*query.TotalStats, error) {
			calls.Add(1)
			return &query.TotalStats{}, nil
		},
	}
	policy := newInvocationPolicy(0, 1, httpConcurrentToolCalls)
	require.True(t, policy.acquire(), "the policy must start with its one-token burst")
	policy.release()
	handler := newMCPHTTPServerWithPolicy(ServeOptions{Engine: engine}, HTTPOptions{}, policy).Handler

	for id := 1; id <= 2; id++ {
		body := task3ToolCallBody(id, ToolGetStats, `{}`)
		recorder, response := task3Serve(handler, task3ModernRequest("tools/call", ToolGetStats, body))
		task3RequireSuccess(t, recorder, response)
		assert.Equal(true, response.Result["isError"], "result: %#v", response.Result)
		assert.Equal("server is busy; retry this tool call later", task3ToolErrorText(t, response))
	}
	assert.Equal(int32(0), calls.Load(), "rejected calls from separate HTTP requests must not invoke their handlers")

	discoverBody := `{"jsonrpc":"2.0","id":42,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	recorder, response := task3Serve(handler, task3ModernRequest("server/discover", "", discoverBody))
	task3RequireSuccess(t, recorder, response)
	listBody := `{"jsonrpc":"2.0","id":43,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	recorder, response = task3Serve(handler, task3ModernRequest("tools/list", "", listBody))
	task3RequireSuccess(t, recorder, response)
}

func TestMCPInvocationLimitTransportPolicies(t *testing.T) {
	tests := []struct {
		name           string
		policy         *invocationPolicy
		wantRate       float64
		wantBurst      int
		wantConcurrent int
	}{
		{
			name:           "HTTP",
			policy:         newHTTPInvocationPolicy(),
			wantRate:       20,
			wantBurst:      40,
			wantConcurrent: 8,
		},
		{
			name:           "stdio",
			policy:         newStdioInvocationPolicy(),
			wantRate:       100,
			wantBurst:      100,
			wantConcurrent: 16,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.InDelta(t, test.wantRate, float64(test.policy.limiter.Limit()), 0)
			assert.Equal(t, test.wantBurst, test.policy.limiter.Burst())
			assert.Equal(t, test.wantConcurrent, cap(test.policy.semaphore))
		})
	}
}

func TestMCPInvocationLimitConcurrentCallsShareHTTPPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	started := make(chan struct{}, 9)
	release := make(chan struct{})
	var calls atomic.Int32
	engine := &querytest.MockEngine{
		GetTotalStatsFunc: func(ctx context.Context, _ query.StatsOptions) (*query.TotalStats, error) {
			calls.Add(1)
			started <- struct{}{}
			select {
			case <-release:
				return &query.TotalStats{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	handler := newMCPHTTPServer(ServeOptions{Engine: engine}, HTTPOptions{}).Handler

	var wait sync.WaitGroup
	wait.Add(8)
	for id := 1; id <= 8; id++ {
		go func() {
			defer wait.Done()
			body := task3ToolCallBody(id, ToolGetStats, `{}`)
			_, _ = task3Serve(handler, task3ModernRequest("tools/call", ToolGetStats, body))
		}()
	}
	for range 8 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			require.FailNow("eight blocking handlers did not start")
		}
	}

	ninthDone := make(chan struct{})
	var ninthRecorder *httptest.ResponseRecorder
	var ninthResponse task3RPCResponse
	go func() {
		defer close(ninthDone)
		body := task3ToolCallBody(9, ToolGetStats, `{}`)
		ninthRecorder, ninthResponse = task3Serve(
			handler,
			task3ModernRequest("tools/call", ToolGetStats, body),
		)
	}()

	ninthInvoked := false
	select {
	case <-ninthDone:
	case <-started:
		ninthInvoked = true
	case <-time.After(2 * time.Second):
	}
	close(release)
	wait.Wait()
	select {
	case <-ninthDone:
	case <-time.After(5 * time.Second):
		require.FailNow("ninth request did not finish")
	}

	require.NotNil(ninthRecorder)
	task3RequireSuccess(t, ninthRecorder, ninthResponse)
	assert.False(ninthInvoked, "the ninth concurrent request must not invoke its handler")
	assert.Equal(int32(8), calls.Load())
	assert.Equal(true, ninthResponse.Result["isError"], "result: %#v", ninthResponse.Result)
	assert.Equal("server is busy; retry this tool call later", task3ToolErrorText(t, ninthResponse))
}
