package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/pkg/client/generated"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	task4TraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	task4TraceState  = "vendor=value"
	task4Baggage     = "tenant=test"
)

func task4RawRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	params map[string]any,
	metaOverrides map[string]any,
) (rawRPCResponse, string) {
	t.Helper()
	must := require.New(t)

	requestParams := make(map[string]any, len(params)+1)
	maps.Copy(requestParams, params)
	meta := modernRequestMeta()
	maps.Copy(meta, metaOverrides)
	requestParams["_meta"] = meta

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  requestParams,
	})
	must.NoError(err)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	if name, _ := requestParams["name"].(string); name != "" {
		req.Header.Set("Mcp-Name", name)
	} else if method == "resources/read" {
		uri, _ := requestParams["uri"].(string)
		req.Header.Set("Mcp-Name", uri)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	var response rawRPCResponse
	must.NoError(json.Unmarshal(recorder.Body.Bytes(), &response), "response: %s", recorder.Body.String())
	return response, recorder.Body.String()
}

func task4ResourceOptions(data []byte) ServeOptions {
	const contentHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return ServeOptions{
		Engine: &querytest.MockEngine{
			Attachments: map[int64]*query.AttachmentInfo{
				7: {
					ID:          7,
					Filename:    "private-report.txt",
					MimeType:    "text/plain",
					Size:        int64(len(data)),
					ContentHash: contentHash,
				},
			},
		},
		AttachmentReader: attachmentReaderFunc(func(context.Context, string) ([]byte, error) {
			return data, nil
		}),
	}
}

func TestAttachmentResourceDiscoveryAndToolUseOpaqueURI(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	opts := task4ResourceOptions([]byte("secret attachment bytes"))
	handler := newMCPHTTPServer(opts, HTTPOptions{}).Handler

	listResponse, listRaw := task4RawRequest(t, handler, "resources/list", nil, nil)
	must.Empty(listResponse.Error, "response: %s", listRaw)
	checks.Equal([]any{}, listResponse.Result["resources"])
	checks.InDelta(float64(300_000), listResponse.Result["ttlMs"], 0)
	checks.Equal("public", listResponse.Result["cacheScope"])

	templateResponse, templateRaw := task4RawRequest(t, handler, "resources/templates/list", nil, nil)
	must.Empty(templateResponse.Error, "response: %s", templateRaw)
	templates, ok := templateResponse.Result["resourceTemplates"].([]any)
	must.True(ok, "resource templates: %#v", templateResponse.Result)
	must.Len(templates, 1)
	template, ok := templates[0].(map[string]any)
	must.True(ok, "resource template: %#v", templates[0])
	checks.Equal("msgvault://attachment/{id}", template["uriTemplate"])
	checks.InDelta(float64(300_000), templateResponse.Result["ttlMs"], 0)
	checks.Equal("public", templateResponse.Result["cacheScope"])
	checks.NotContains(templateRaw, "private-report.txt")

	toolResponse, toolRaw := task4RawRequest(t, handler, "tools/call", map[string]any{
		"name": ToolGetAttachment,
		"arguments": map[string]any{
			"attachment_id": 7,
		},
	}, nil)
	must.Empty(toolResponse.Error, "response: %s", toolRaw)
	structured, ok := toolResponse.Result["structuredContent"].(map[string]any)
	must.True(ok, "structured content: %#v", toolResponse.Result)
	checks.Equal("private-report.txt", structured["filename"])
	content, ok := toolResponse.Result["content"].([]any)
	must.True(ok, "content: %#v", toolResponse.Result)
	must.Len(content, 2)
	embedded, ok := content[1].(map[string]any)
	must.True(ok, "embedded content: %#v", content[1])
	resource, ok := embedded["resource"].(map[string]any)
	must.True(ok, "embedded resource: %#v", embedded)
	checks.Equal("msgvault://attachment/7", resource["uri"])
	checks.NotContains(resource["uri"], "private-report.txt")
}

func TestAttachmentResourceReadReturnsBoundedBytes(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	data := []byte("exact attachment bytes")
	handler := newMCPHTTPServer(task4ResourceOptions(data), HTTPOptions{}).Handler

	response, raw := task4RawRequest(t, handler, "resources/read", map[string]any{
		"uri": "msgvault://attachment/7",
	}, nil)
	must.Empty(response.Error, "response: %s", raw)
	contents, ok := response.Result["contents"].([]any)
	must.True(ok, "contents: %#v", response.Result)
	must.Len(contents, 1)
	resource, ok := contents[0].(map[string]any)
	must.True(ok, "resource: %#v", contents[0])
	checks.Equal("msgvault://attachment/7", resource["uri"])
	checks.Equal("text/plain", resource["mimeType"])
	checks.Equal(base64.StdEncoding.EncodeToString(data), resource["blob"])
	checks.InDelta(float64(60_000), response.Result["ttlMs"], 0)
	checks.Equal("private", response.Result["cacheScope"])
}

func TestAttachmentResourceRejectsNonCanonicalAndUnavailableReads(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	handler := newMCPHTTPServer(task4ResourceOptions([]byte("data")), HTTPOptions{}).Handler
	invalidURIs := []string{
		"msgvault://attachment/",
		"msgvault://attachment/0",
		"msgvault://attachment/01",
		"msgvault://attachment/+1",
		"msgvault://attachment/-1",
		"msgvault://attachment/1.0",
		"msgvault://attachment/9007199254740992",
		"msgvault://attachment/1/extra",
		"msgvault://attachment/%31",
		"msgvault://attachment/1?x=y",
		"msgvault://attachment/1#x",
		"msgvault://user@attachment/1",
		"other://attachment/1",
		"msgvault://other/1",
		"msgvault:attachment/1",
	}
	for _, uri := range invalidURIs {
		t.Run(uri, func(t *testing.T) {
			response, raw := task4RawRequest(t, handler, "resources/read", map[string]any{"uri": uri}, nil)
			checks.InDelta(float64(jsonrpc.CodeInvalidParams), response.Error["code"], 0, "response: %s", raw)
		})
	}

	unknown, unknownRaw := task4RawRequest(t, handler, "resources/read", map[string]any{
		"uri": "msgvault://attachment/8",
	}, nil)
	checks.InDelta(float64(jsonrpc.CodeInvalidParams), unknown.Error["code"], 0, "response: %s", unknownRaw)

	unavailableOpts := ServeOptions{Engine: task4ResourceOptions([]byte("data")).Engine}
	unavailable, unavailableRaw := task4RawRequest(
		t,
		newMCPHTTPServer(unavailableOpts, HTTPOptions{}).Handler,
		"resources/read",
		map[string]any{"uri": "msgvault://attachment/7"},
		nil,
	)
	checks.InDelta(float64(jsonrpc.CodeInvalidParams), unavailable.Error["code"], 0, "response: %s", unavailableRaw)

	var reads int
	oversizedOpts := task4ResourceOptions([]byte("must not be read"))
	oversizedOpts.Engine = &querytest.MockEngine{Attachments: map[int64]*query.AttachmentInfo{
		9: {
			ID:          9,
			Filename:    "large.bin",
			Size:        maxAttachmentSize + 1,
			ContentHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}}
	oversizedOpts.AttachmentReader = attachmentReaderFunc(func(context.Context, string) ([]byte, error) {
		reads++
		return []byte("must not be read"), nil
	})
	oversized, oversizedRaw := task4RawRequest(
		t,
		newMCPHTTPServer(oversizedOpts, HTTPOptions{}).Handler,
		"resources/read",
		map[string]any{"uri": "msgvault://attachment/9"},
		nil,
	)
	checks.InDelta(float64(jsonrpc.CodeInvalidParams), oversized.Error["code"], 0, "response: %s", oversizedRaw)
	checks.Zero(reads, "oversized metadata must reject before reading bytes")
	must.NotContains(oversizedRaw, "private-report.txt")
}

func TestAttachmentResourceDaemonNotFoundIsInvalidParams(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Equal("/api/v1/cli/attachment", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, err := w.Write([]byte(`{"error":"attachment_not_found","message":"attachment bytes unavailable"}`))
		checks.NoError(err)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL:           daemon.URL,
		AllowInsecure: true,
		HTTPClient:    daemon.Client(),
	})
	must.NoError(err)

	opts := task4ResourceOptions([]byte("unused"))
	opts.AttachmentReader = client
	response, raw := task4RawRequest(
		t,
		newMCPHTTPServer(opts, HTTPOptions{}).Handler,
		"resources/read",
		map[string]any{"uri": "msgvault://attachment/7"},
		nil,
	)

	checks.InDelta(float64(jsonrpc.CodeInvalidParams), response.Error["code"], 0, "response: %s", raw)
	checks.NotContains(raw, "attachment bytes unavailable")
}

func TestMCPCachePolicyOverwritesSDKDefaultsByMethod(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	opts := task4ResourceOptions([]byte("data"))
	engine, ok := opts.Engine.(*querytest.MockEngine)
	must.True(ok)
	engine.Stats = &query.TotalStats{}
	handler := newMCPHTTPServer(opts, HTTPOptions{}).Handler

	tests := []struct {
		method string
		params map[string]any
		ttl    float64
		scope  string
	}{
		{method: "server/discover", ttl: 3_600_000, scope: "public"},
		{method: "tools/list", ttl: 300_000, scope: "public"},
		{method: "resources/list", ttl: 300_000, scope: "public"},
		{method: "resources/templates/list", ttl: 300_000, scope: "public"},
		{method: "resources/read", params: map[string]any{"uri": "msgvault://attachment/7"}, ttl: 60_000, scope: "private"},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			subChecks := assert.New(t)
			subMust := require.New(t)
			response, raw := task4RawRequest(t, handler, test.method, test.params, nil)
			subMust.Empty(response.Error, "response: %s", raw)
			subChecks.InDelta(test.ttl, response.Result["ttlMs"], 0)
			subChecks.Equal(test.scope, response.Result["cacheScope"])
		})
	}

	toolResponse, toolRaw := task4RawRequest(t, handler, "tools/call", map[string]any{
		"name":      ToolGetStats,
		"arguments": map[string]any{},
	}, nil)
	must.Empty(toolResponse.Error, "response: %s", toolRaw)
	checks.NotContains(toolResponse.Result, "ttlMs")
	checks.NotContains(toolResponse.Result, "cacheScope")
}

type task4FixedIDGenerator struct{}

func (task4FixedIDGenerator) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	return trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
}

func (task4FixedIDGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	return trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
}

func TestMCPTracePropagationCreatesServerSpanAndReachesDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(task4FixedIDGenerator{}),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		must.NoError(provider.Shutdown(context.Background()))
	})

	headers := make(chan http.Header, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL:           daemon.URL,
		AllowInsecure: true,
		HTTPClient:    daemon.Client(),
	})
	must.NoError(err)

	opts := ServeOptions{
		Engine: &querytest.MockEngine{},
		HybridSearcher: hybridSearcherFunc(func(ctx context.Context, _ HybridSearchRequest) (*HybridSearchResult, error) {
			resp, requestErr := client.DoGeneratedRequestWithContext(
				ctx,
				http.MethodGet,
				"/trace",
				&generated.RunCLIRequestOptions{},
			)
			if resp != nil {
				_ = resp.Body.Close()
			}
			return &HybridSearchResult{}, requestErr
		}),
	}
	response, raw := task4RawRequest(
		t,
		newMCPHTTPServer(opts, HTTPOptions{}).Handler,
		"tools/call",
		map[string]any{
			"name": ToolSemanticSearchMessages,
			"arguments": map[string]any{
				"query": "needle",
				"mode":  searchModeHybrid,
			},
		},
		map[string]any{
			"traceparent": task4TraceParent,
			"tracestate":  task4TraceState,
			"baggage":     task4Baggage,
			"ignored":     map[string]any{"secret": true},
		},
	)
	must.Empty(response.Error, "response: %s", raw)

	gotHeaders := <-headers
	checks.Equal("00-4bf92f3577b34da6a3ce929d0e0e4736-0102030405060708-01", gotHeaders.Get("Traceparent"))
	checks.Equal(task4TraceState, gotHeaders.Get("Tracestate"))
	checks.Equal(task4Baggage, gotHeaders.Get("Baggage"))

	ended := recorder.Ended()
	must.Len(ended, 1)
	span := ended[0]
	checks.Equal("tools/call", span.Name())
	checks.Equal(trace.SpanKindServer, span.SpanKind())
	checks.Equal("4bf92f3577b34da6a3ce929d0e0e4736", span.SpanContext().TraceID().String())
	checks.Equal("00f067aa0ba902b7", span.Parent().SpanID().String())
	checks.True(span.Parent().IsRemote())
}

func TestMCPTracePropagationIgnoresNonStringMetadata(t *testing.T) {
	response, raw := task4RawRequest(
		t,
		newMCPHTTPServer(ServeOptions{
			Engine: &querytest.MockEngine{Stats: &query.TotalStats{}},
		}, HTTPOptions{}).Handler,
		"tools/call",
		map[string]any{"name": ToolGetStats, "arguments": map[string]any{}},
		map[string]any{
			"traceparent": map[string]any{"not": "a string"},
			"tracestate":  7,
			"baggage":     []any{"not", "a string"},
		},
	)
	require.New(t).Empty(response.Error, "response: %s", raw)
}

type task4AttachmentErrorEngine struct {
	*querytest.MockEngine

	err error
}

type task4DaemonHybridErrorSearcher struct {
	client *daemonclient.Client
}

func (s task4DaemonHybridErrorSearcher) SearchHybrid(
	ctx context.Context,
	req HybridSearchRequest,
) (*HybridSearchResult, error) {
	_, err := s.client.GetCLIHybridSearch(ctx, daemonclient.CLIHybridSearchRequest{
		Query:          req.Query,
		Account:        req.Account,
		Mode:           req.Mode,
		Limit:          req.Limit,
		Offset:         req.Offset,
		IncludeMatches: req.IncludeMatches,
		MinScore:       req.MinScore,
	})
	return nil, err
}

type task4DaemonSimilarErrorSearcher struct {
	client *daemonclient.Client
}

func (s task4DaemonSimilarErrorSearcher) FindSimilar(
	ctx context.Context,
	req SimilarSearchRequest,
) (*SimilarSearchResult, error) {
	_, err := s.client.FindSimilarMessages(ctx, daemonclient.SimilarSearchRequest{
		MessageID:     req.MessageID,
		Limit:         req.Limit,
		Account:       req.Account,
		MessageType:   req.MessageType,
		After:         req.After,
		Before:        req.Before,
		HasAttachment: req.HasAttachment,
	})
	return nil, err
}

func task4DaemonErrorClient(t *testing.T, path, code, message string, status int) *daemonclient.Client {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, path, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message}))
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL:           daemon.URL,
		AllowInsecure: true,
		HTTPClient:    daemon.Client(),
	})
	require.NoError(t, err)
	return client
}

func TestMCPInternalErrorIsolationMapsOnlyKnownDaemonVectorCodes(t *testing.T) {
	vectorCodes := []struct {
		code   string
		status int
	}{
		{code: "vector_not_enabled", status: http.StatusServiceUnavailable},
		{code: "index_stale", status: http.StatusServiceUnavailable},
		{code: "index_building", status: http.StatusServiceUnavailable},
		{code: "embedding_timeout", status: http.StatusServiceUnavailable},
		{code: "index_scope_mismatch", status: http.StatusBadRequest},
	}
	paths := []struct {
		name string
		path string
		tool string
		args map[string]any
		opts func(*daemonclient.Client) ServeOptions
	}{
		{
			name: "hybrid adapter",
			path: "/api/v1/search",
			tool: ToolSemanticSearchMessages,
			args: map[string]any{"query": "needle", "mode": searchModeHybrid},
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{
					Engine:         &querytest.MockEngine{},
					HybridSearcher: task4DaemonHybridErrorSearcher{client: client},
				}
			},
		},
		{
			name: "similar adapter",
			path: "/api/v1/search/similar",
			tool: ToolFindSimilarMessages,
			args: map[string]any{"message_id": 1},
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{
					Engine:          &querytest.MockEngine{},
					SimilarSearcher: task4DaemonSimilarErrorSearcher{client: client},
				}
			},
		},
	}

	for _, path := range paths {
		for _, vectorCode := range vectorCodes {
			t.Run(path.name+"/"+vectorCode.code, func(t *testing.T) {
				checks := assert.New(t)
				must := require.New(t)
				privateMessage := "daemon-private-" + vectorCode.code
				client := task4DaemonErrorClient(t, path.path, vectorCode.code, privateMessage, vectorCode.status)
				response, raw := task4RawRequest(
					t,
					newMCPHTTPServer(path.opts(client), HTTPOptions{}).Handler,
					"tools/call",
					map[string]any{"name": path.tool, "arguments": path.args},
					nil,
				)

				must.Empty(response.Error, "response: %s", raw)
				checks.Equal(true, response.Result["isError"])
				checks.Contains(raw, vectorCode.code)
				checks.NotContains(raw, privateMessage)
			})
		}
	}

	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for _, path := range paths {
		t.Run(path.name+"/unknown", func(t *testing.T) {
			checks := assert.New(t)
			logs.Reset()
			privateMessage := "daemon-private-unknown-" + strings.ReplaceAll(path.name, " ", "-")
			client := task4DaemonErrorClient(
				t, path.path, "private_backend_failure", privateMessage, http.StatusInternalServerError,
			)
			response, raw := task4RawRequest(
				t,
				newMCPHTTPServer(path.opts(client), HTTPOptions{}).Handler,
				"tools/call",
				map[string]any{"name": path.tool, "arguments": path.args},
				nil,
			)

			checks.InDelta(float64(jsonrpc.CodeInternalError), response.Error["code"], 0, "response: %s", raw)
			checks.Equal("internal server error", response.Error["message"])
			checks.NotContains(raw, privateMessage)
			checks.Contains(logs.String(), privateMessage)
		})
	}
}

func TestMCPDaemonRequestErrorsBecomeSafeToolResults(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		code   string
		status int
		tool   string
		args   map[string]any
		want   string
		opts   func(*daemonclient.Client) ServeOptions
	}{
		{
			name: "body invalid query", path: "/api/v1/search/deep", code: "invalid_query",
			status: http.StatusBadRequest, tool: ToolSearchMessageBodies,
			args: map[string]any{"query": "needle"}, want: "invalid_query: search query is invalid",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: daemonclient.NewEngineAdapter(client)}
			},
		},
		{
			name: "body search unavailable", path: "/api/v1/search/deep", code: "body_search_unavailable",
			status: http.StatusNotImplemented, tool: ToolSearchMessageBodies,
			args: map[string]any{"query": "needle"},
			want: "body_search_unavailable: exact message body search is unavailable",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: daemonclient.NewEngineAdapter(client)}
			},
		},
		{
			name: "body search index unavailable", path: "/api/v1/search/deep",
			code: "body_search_index_unavailable", status: http.StatusServiceUnavailable,
			tool: ToolSearchMessageBodies, args: map[string]any{"query": "needle"},
			want: "body_search_index_unavailable: message body search index is unavailable",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: daemonclient.NewEngineAdapter(client)}
			},
		},
		{
			name: "hybrid invalid account", path: "/api/v1/search", code: "invalid_account",
			status: http.StatusBadRequest, tool: ToolSemanticSearchMessages,
			args: map[string]any{"query": "needle", "mode": searchModeHybrid},
			want: "invalid_account: account filter is invalid",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: task4DaemonHybridErrorSearcher{client}}
			},
		},
		{
			name: "hybrid account not found", path: "/api/v1/search", code: "account_not_found",
			status: http.StatusNotFound, tool: ToolSemanticSearchMessages,
			args: map[string]any{"query": "needle", "mode": searchModeHybrid},
			want: "account_not_found: requested account was not found",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: task4DaemonHybridErrorSearcher{client}}
			},
		},
		{
			name: "hybrid pagination unsupported", path: "/api/v1/search", code: "pagination_unsupported",
			status: http.StatusBadRequest, tool: ToolSemanticSearchMessages,
			args: map[string]any{"query": "needle", "mode": searchModeHybrid},
			want: "pagination_unsupported: this search mode does not support the requested page",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: task4DaemonHybridErrorSearcher{client}}
			},
		},
		{
			name: "hybrid pagination limit", path: "/api/v1/search", code: "pagination_limit",
			status: http.StatusBadRequest, tool: ToolSemanticSearchMessages,
			args: map[string]any{"query": "needle", "mode": searchModeHybrid},
			want: "pagination_limit: requested offset exceeds the available search window",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: task4DaemonHybridErrorSearcher{client}}
			},
		},
		{
			name: "similar invalid message", path: "/api/v1/search/similar", code: "invalid_message_id",
			status: http.StatusBadRequest, tool: ToolFindSimilarMessages,
			args: map[string]any{"message_id": 1}, want: "invalid_message_id: seed message ID is invalid",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: &querytest.MockEngine{}, SimilarSearcher: task4DaemonSimilarErrorSearcher{client}}
			},
		},
		{
			name: "similar invalid limit", path: "/api/v1/search/similar", code: "invalid_limit",
			status: http.StatusBadRequest, tool: ToolFindSimilarMessages,
			args: map[string]any{"message_id": 1}, want: "invalid_limit: result limit is invalid",
			opts: func(client *daemonclient.Client) ServeOptions {
				return ServeOptions{Engine: &querytest.MockEngine{}, SimilarSearcher: task4DaemonSimilarErrorSearcher{client}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			privateMessage := "daemon-private-" + strings.ReplaceAll(test.name, " ", "-")
			client := task4DaemonErrorClient(t, test.path, test.code, privateMessage, test.status)
			response, raw := task4RawRequest(
				t,
				newMCPHTTPServer(test.opts(client), HTTPOptions{}).Handler,
				"tools/call",
				map[string]any{"name": test.tool, "arguments": test.args},
				nil,
			)

			must.Empty(response.Error, "response: %s", raw)
			checks.Equal(true, response.Result["isError"])
			checks.Contains(raw, test.want)
			checks.NotContains(raw, privateMessage)
		})
	}
}

func TestMCPDaemonBodySearchUnknownCodeStaysPrivate(t *testing.T) {
	const privateMessage = "daemon-private-body-search-failure"
	logs := task6CaptureLogs(t)
	client := task4DaemonErrorClient(
		t,
		"/api/v1/search/deep",
		"private_backend_failure",
		privateMessage,
		http.StatusInternalServerError,
	)
	response, raw := task4RawRequest(
		t,
		newMCPHTTPServer(
			ServeOptions{Engine: daemonclient.NewEngineAdapter(client)},
			HTTPOptions{},
		).Handler,
		"tools/call",
		map[string]any{
			"name":      ToolSearchMessageBodies,
			"arguments": map[string]any{"query": "needle"},
		},
		nil,
	)

	checks := assert.New(t)
	checks.InDelta(float64(jsonrpc.CodeInternalError), response.Error["code"], 0, "response: %s", raw)
	checks.Equal("internal server error", response.Error["message"])
	checks.NotContains(raw, privateMessage)
	checks.Contains(logs.String(), privateMessage)
}

func TestMCPInternalErrorIsolationKeepsCancellationPrivate(t *testing.T) {
	checks := assert.New(t)
	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	opts := ServeOptions{Engine: &querytest.MockEngine{
		GetTotalStatsFunc: func(context.Context, query.StatsOptions) (*query.TotalStats, error) {
			return nil, context.Canceled
		},
	}}

	response, raw := task4RawRequest(
		t,
		newMCPHTTPServer(opts, HTTPOptions{}).Handler,
		"tools/call",
		map[string]any{"name": ToolGetStats, "arguments": map[string]any{}},
		nil,
	)

	checks.InDelta(float64(jsonrpc.CodeInternalError), response.Error["code"], 0, "response: %s", raw)
	checks.Equal("internal server error", response.Error["message"])
	checks.NotContains(raw, context.Canceled.Error())
	checks.Contains(logs.String(), context.Canceled.Error())
}

func (e *task4AttachmentErrorEngine) GetAttachment(context.Context, int64) (*query.AttachmentInfo, error) {
	return nil, e.err
}

type task4FailingManifestSaver struct {
	err error
}

func (s task4FailingManifestSaver) SaveManifest(context.Context, *deletion.Manifest) error {
	return s.err
}

type task4FailingExportFile struct {
	writeErr error
	closeErr error
}

func (f *task4FailingExportFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(data), nil
}

func (f *task4FailingExportFile) Close() error {
	return f.closeErr
}

func TestMCPInternalErrorIsolationCoversExportWriteAndClose(t *testing.T) {
	tests := []struct {
		name     string
		sentinel string
		file     *task4FailingExportFile
	}{
		{
			name:     "write",
			sentinel: "task4-export-write-private",
			file:     &task4FailingExportFile{writeErr: errors.New("task4-export-write-private")},
		},
		{
			name:     "close",
			sentinel: "task4-export-close-private",
			file:     &task4FailingExportFile{closeErr: errors.New("task4-export-close-private")},
		},
	}

	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	originalCreate := createAttachmentExportFile
	t.Cleanup(func() { createAttachmentExportFile = originalCreate })

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			logs.Reset()
			createAttachmentExportFile = func(string, os.FileMode) (attachmentExportFile, string, error) {
				return test.file, filepath.Join(t.TempDir(), "failed-export"), nil
			}
			response, raw := task4RawRequest(
				t,
				newMCPHTTPServer(task4ResourceOptions([]byte("x")), HTTPOptions{AllowWrites: true}).Handler,
				"tools/call",
				map[string]any{
					"name": ToolExportAttachment,
					"arguments": map[string]any{
						"attachment_id": 7,
						"destination":   t.TempDir(),
					},
				},
				nil,
			)

			checks.InDelta(float64(jsonrpc.CodeInternalError), response.Error["code"], 0, "response: %s", raw)
			checks.Equal("internal server error", response.Error["message"])
			checks.NotContains(raw, test.sentinel)
			checks.Contains(logs.String(), test.sentinel)
		})
	}
}

type task4MarshalFailure struct {
	err error
}

func (f task4MarshalFailure) MarshalJSON() ([]byte, error) {
	return nil, f.err
}

func task4SerializationFailureHandler(sentinel string) http.Handler {
	server := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "msgvault-test", Version: "1.0.0"},
		&sdkmcp.ServerOptions{
			Capabilities: &sdkmcp.ServerCapabilities{Tools: &sdkmcp.ToolCapabilities{}},
			SchemaCache:  mcpSchemaCache,
		},
	)
	sdkmcp.AddTool[map[string]any, any](server, &sdkmcp.Tool{
		Name:        "task4_marshal_failure",
		InputSchema: closedObject(map[string]*jsonschema.Schema{}),
	}, officialToolHandler(func(context.Context, toolRequest) (*toolResult, error) {
		return jsonResult(task4MarshalFailure{err: errors.New(sentinel)})
	}))
	return sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
}

func TestMCPInternalErrorIsolationCoversEveryDependencyClass(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const contentHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	exportDir := t.TempDir()

	type testCase struct {
		name     string
		tool     string
		args     map[string]any
		opts     func(error) ServeOptions
		httpOpts HTTPOptions
	}
	tests := []testCase{
		{
			name: "query engine",
			tool: ToolGetStats,
			args: map[string]any{},
			opts: func(failure error) ServeOptions {
				return ServeOptions{Engine: &querytest.MockEngine{
					GetTotalStatsFunc: func(context.Context, query.StatsOptions) (*query.TotalStats, error) {
						return nil, failure
					},
				}}
			},
		},
		{
			name: "attachment reader",
			tool: ToolGetAttachment,
			args: map[string]any{"attachment_id": 1},
			opts: func(failure error) ServeOptions {
				return ServeOptions{
					Engine: &querytest.MockEngine{Attachments: map[int64]*query.AttachmentInfo{
						1: {ID: 1, Filename: "file.bin", Size: 1, ContentHash: contentHash},
					}},
					AttachmentReader: attachmentReaderFunc(func(context.Context, string) ([]byte, error) {
						return nil, failure
					}),
				}
			},
		},
		{
			name: "attachment metadata query",
			tool: ToolGetAttachment,
			args: map[string]any{"attachment_id": 1},
			opts: func(failure error) ServeOptions {
				return ServeOptions{Engine: &task4AttachmentErrorEngine{
					MockEngine: &querytest.MockEngine{},
					err:        failure,
				}}
			},
		},
		{
			name:     "export filesystem create",
			tool:     ToolExportAttachment,
			args:     map[string]any{"attachment_id": 1, "destination": exportDir},
			httpOpts: HTTPOptions{AllowWrites: true},
			opts: func(failure error) ServeOptions {
				failureName := strings.Repeat("x", 300) + failure.Error()
				return ServeOptions{
					Engine: &querytest.MockEngine{Attachments: map[int64]*query.AttachmentInfo{
						1: {ID: 1, Filename: failureName, Size: 1, ContentHash: contentHash},
					}},
					AttachmentReader: attachmentReaderFunc(func(context.Context, string) ([]byte, error) {
						return []byte("x"), nil
					}),
				}
			},
		},
		{
			name:     "deletion manifest saver",
			tool:     ToolStageDeletion,
			args:     map[string]any{"domain": "example.test"},
			httpOpts: HTTPOptions{AllowWrites: true},
			opts: func(failure error) ServeOptions {
				return ServeOptions{
					Engine:        &querytest.MockEngine{GmailIDs: []string{"message-1"}},
					ManifestSaver: task4FailingManifestSaver{err: failure},
				}
			},
		},
		{
			name: "daemon hybrid searcher",
			tool: ToolSemanticSearchMessages,
			args: map[string]any{"query": "needle", "mode": searchModeHybrid},
			opts: func(failure error) ServeOptions {
				return ServeOptions{
					Engine: &querytest.MockEngine{},
					HybridSearcher: hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
						return nil, failure
					}),
				}
			},
		},
		{
			name: "daemon similar searcher",
			tool: ToolFindSimilarMessages,
			args: map[string]any{"message_id": 1},
			opts: func(failure error) ServeOptions {
				return ServeOptions{
					Engine: &querytest.MockEngine{},
					SimilarSearcher: similarSearcherFunc(func(context.Context, SimilarSearchRequest) (*SimilarSearchResult, error) {
						return nil, failure
					}),
				}
			},
		},
		{
			name: "in-process vector backend",
			tool: ToolFindSimilarMessages,
			args: map[string]any{"message_id": 1},
			opts: func(failure error) ServeOptions {
				cfg := testSimilarVectorConfig()
				return ServeOptions{
					Engine:    &querytest.MockEngine{},
					Backend:   &fakeBackend{active: testSimilarActiveGeneration(cfg), loadErr: failure},
					VectorCfg: cfg,
				}
			},
		},
	}

	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs.Reset()
			sentinel := "task4-secret-" + strings.ReplaceAll(test.name, " ", "-")
			response, raw := task4RawRequest(
				t,
				newMCPHTTPServer(test.opts(errors.New(sentinel)), test.httpOpts).Handler,
				"tools/call",
				map[string]any{"name": test.tool, "arguments": test.args},
				nil,
			)
			checks.InDelta(float64(jsonrpc.CodeInternalError), response.Error["code"], 0, "response: %s", raw)
			checks.Equal("internal server error", response.Error["message"], "response: %s", raw)
			checks.NotContains(raw, sentinel)
			checks.Contains(logs.String(), sentinel)
		})
	}

	logs.Reset()
	serializationSentinel := "task4-secret-json-serialization"
	serialization, serializationRaw := task4RawRequest(
		t,
		task4SerializationFailureHandler(serializationSentinel),
		"tools/call",
		map[string]any{"name": "task4_marshal_failure", "arguments": map[string]any{}},
		nil,
	)
	checks.InDelta(float64(jsonrpc.CodeInternalError), serialization.Error["code"], 0, "response: %s", serializationRaw)
	checks.Equal("internal server error", serialization.Error["message"], "response: %s", serializationRaw)
	checks.NotContains(serializationRaw, serializationSentinel)
	checks.Contains(logs.String(), serializationSentinel)
	must.NotContains(serializationRaw, "marshal tool result")
}

func TestMCPInternalErrorIsolationPreservesSafeControls(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)

	invalid, invalidRaw := task4RawRequest(
		t,
		newMCPHTTPServer(ServeOptions{Engine: &querytest.MockEngine{}}, HTTPOptions{}).Handler,
		"tools/call",
		map[string]any{
			"name":      ToolSearchMetadata,
			"arguments": map[string]any{"query": ""},
		},
		nil,
	)
	must.Empty(invalid.Error, "response: %s", invalidRaw)
	checks.Equal(true, invalid.Result["isError"])
	checks.Contains(invalidRaw, "query parameter is required")

	notFound, notFoundRaw := task4RawRequest(
		t,
		newMCPHTTPServer(ServeOptions{Engine: &querytest.MockEngine{}}, HTTPOptions{}).Handler,
		"tools/call",
		map[string]any{
			"name":      ToolGetAttachment,
			"arguments": map[string]any{"attachment_id": 99},
		},
		nil,
	)
	must.Empty(notFound.Error, "response: %s", notFoundRaw)
	checks.Equal(true, notFound.Result["isError"])
	checks.Contains(notFoundRaw, "attachment not found")

	cfg := testSimilarVectorConfig()
	vectorSentinel, vectorRaw := task4RawRequest(
		t,
		newMCPHTTPServer(ServeOptions{
			Engine:    &querytest.MockEngine{},
			Backend:   &fakeBackend{activeErr: vector.ErrNoActiveGeneration},
			VectorCfg: cfg,
		}, HTTPOptions{}).Handler,
		"tools/call",
		map[string]any{
			"name":      ToolFindSimilarMessages,
			"arguments": map[string]any{"message_id": 1},
		},
		nil,
	)
	must.Empty(vectorSentinel.Error, "response: %s", vectorRaw)
	checks.Equal(true, vectorSentinel.Result["isError"])
	checks.Contains(vectorRaw, "vector_not_enabled")
}
