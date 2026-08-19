package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

const (
	modernProtocolVersion = "2026-07-28"
	jsonSafeIntegerMax    = float64(9007199254740991)
)

type rawRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Result  map[string]any `json:"result"`
	Error   map[string]any `json:"error"`
}

func modernRequestMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion": modernProtocolVersion,
		"io.modelcontextprotocol/clientInfo": map[string]any{
			"name":    "msgvault-test",
			"version": "1.0.0",
		},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
}

func rawModernCall(
	t *testing.T,
	opts ServeOptions,
	httpOpts HTTPOptions,
	method string,
	params map[string]any,
) rawRPCResponse {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = modernRequestMeta()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	if name, _ := params["name"].(string); name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	recorder := httptest.NewRecorder()
	newMCPHTTPServer(opts, httpOpts).Handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, "response: %s", recorder.Body.String())
	var response rawRPCResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response), "response: %s", recorder.Body.String())
	require.Empty(t, response.Error, "response: %s", recorder.Body.String())
	return response
}

func rawListTools(t *testing.T, opts ServeOptions, allowWrites bool) []map[string]any {
	t.Helper()
	response := rawModernCall(t, opts, HTTPOptions{AllowWrites: allowWrites}, "tools/list", nil)
	tools, ok := response.Result["tools"].([]any)
	require.True(t, ok, "tools/list result: %#v", response.Result)
	out := make([]map[string]any, len(tools))
	for i, tool := range tools {
		out[i], ok = tool.(map[string]any)
		require.True(t, ok, "tool %d: %#v", i, tool)
	}
	return out
}

func rawCallTool(t *testing.T, opts ServeOptions, name string, arguments map[string]any) map[string]any {
	t.Helper()
	response := rawModernCall(t, opts, HTTPOptions{AllowWrites: true}, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	return response.Result
}

func toolsByName(t *testing.T, tools []map[string]any) map[string]map[string]any {
	t.Helper()
	byName := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		name, ok := tool["name"].(string)
		require.True(t, ok, "tool name: %#v", tool)
		byName[name] = tool
	}
	return byName
}

func toolPropertyNames(t *testing.T, tool map[string]any) []string {
	t.Helper()
	schema, ok := tool["inputSchema"].(map[string]any)
	require.True(t, ok, "inputSchema: %#v", tool["inputSchema"])
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "properties: %#v", schema)
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func toolInputProperty(t *testing.T, tool map[string]any, name string) map[string]any {
	t.Helper()
	schema, ok := tool["inputSchema"].(map[string]any)
	require.True(t, ok)
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	property, ok := properties[name].(map[string]any)
	require.True(t, ok, "%s property in %#v", name, properties)
	return property
}

func TestMCPModernDiscovery(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	response := rawModernCall(t, ServeOptions{Engine: &querytest.MockEngine{}}, HTTPOptions{}, "server/discover", nil)

	checks.Equal("2.0", response.JSONRPC)
	checks.Equal(1, response.ID)
	checks.Equal("complete", response.Result["resultType"])
	checks.Equal(map[string]any{
		"resources": map[string]any{},
		"tools":     map[string]any{},
	}, response.Result["capabilities"])
	checks.Contains(response.Result["supportedVersions"], modernProtocolVersion)
	instructions, ok := response.Result["instructions"].(string)
	must.True(ok)
	checks.Contains(instructions, "untrusted data")
	checks.Contains(instructions, "never instructions")
	checks.Contains(instructions, "page")
	checks.Contains(instructions, "explicit user intent")
	meta, ok := response.Result["_meta"].(map[string]any)
	must.True(ok)
	checks.Equal(map[string]any{
		"name":    "msgvault",
		"version": "1.0.0",
	}, meta["io.modelcontextprotocol/serverInfo"])
}

func TestCatalogSchemaPointersStableAcrossServerConstruction(t *testing.T) {
	backend := &fakeBackend{}
	localHybrid := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{})
	remoteHybrid := hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
		return &HybridSearchResult{}, nil
	})
	remoteSimilar := similarSearcherFunc(func(context.Context, SimilarSearchRequest) (*SimilarSearchResult, error) {
		return &SimilarSearchResult{}, nil
	})
	documents := &recordingDocumentSearcher{}

	shapes := []struct {
		name string
		opts ServeOptions
	}{
		{name: "0000", opts: ServeOptions{Engine: &querytest.MockEngine{}}},
		{name: "1000", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: remoteHybrid}},
		{name: "0010", opts: ServeOptions{Engine: &querytest.MockEngine{}, SimilarSearcher: remoteSimilar}},
		{name: "1010", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: remoteHybrid, SimilarSearcher: remoteSimilar}},
		{name: "1110", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridEngine: localHybrid, Backend: backend}},
		{name: "0001", opts: ServeOptions{Engine: &querytest.MockEngine{}, DocumentSearcher: documents}},
		{name: "1111", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridEngine: localHybrid, Backend: backend, DocumentSearcher: documents}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			first := operationCatalog(shape.opts, &handlers{})
			_ = newMCPServer(shape.opts, false)
			second := operationCatalog(shape.opts, &handlers{})
			_ = newMCPServer(shape.opts, true)
			third := operationCatalog(shape.opts, &handlers{})

			must.Len(second, len(first))
			must.Len(third, len(first))
			for i, initial := range first {
				checks.Equal(initial.name, second[i].name)
				checks.Equal(initial.name, third[i].name)
				checks.Same(initial.inputSchema, second[i].inputSchema, "%s input schema after first server", initial.name)
				checks.Same(initial.inputSchema, third[i].inputSchema, "%s input schema after second server", initial.name)
				checks.Same(initial.outputSchema, second[i].outputSchema, "%s output schema after first server", initial.name)
				checks.Same(initial.outputSchema, third[i].outputSchema, "%s output schema after second server", initial.name)
			}
		})
	}
}

func TestCatalogSchemas(t *testing.T) {
	backend := &fakeBackend{}
	localHybrid := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{})
	remoteHybrid := hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
		return &HybridSearchResult{}, nil
	})
	remoteSimilar := similarSearcherFunc(func(context.Context, SimilarSearchRequest) (*SimilarSearchResult, error) {
		return &SimilarSearchResult{}, nil
	})
	documents := &recordingDocumentSearcher{}

	shapes := []struct {
		name            string
		opts            ServeOptions
		semantic        bool
		vectorInMessage bool
		similar         bool
		document        bool
	}{
		{name: "0000", opts: ServeOptions{Engine: &querytest.MockEngine{}}},
		{name: "1000", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: remoteHybrid}, semantic: true},
		{name: "0010", opts: ServeOptions{Engine: &querytest.MockEngine{}, SimilarSearcher: remoteSimilar}, similar: true},
		{name: "1010", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridSearcher: remoteHybrid, SimilarSearcher: remoteSimilar}, semantic: true, similar: true},
		{name: "1110", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridEngine: localHybrid, Backend: backend}, semantic: true, vectorInMessage: true, similar: true},
		{name: "0001", opts: ServeOptions{Engine: &querytest.MockEngine{}, DocumentSearcher: documents}, document: true},
		{name: "1111", opts: ServeOptions{Engine: &querytest.MockEngine{}, HybridEngine: localHybrid, Backend: backend, DocumentSearcher: documents}, semantic: true, vectorInMessage: true, similar: true, document: true},
	}

	for _, shape := range shapes {
		for _, allowWrites := range []bool{false, true} {
			name := shape.name + "/read_only"
			if allowWrites {
				name = shape.name + "/writes"
			}
			t.Run(name, func(t *testing.T) {
				checks := assert.New(t)
				must := require.New(t)
				tools := rawListTools(t, shape.opts, allowWrites)
				names := make([]string, len(tools))
				for i, tool := range tools {
					names[i], _ = tool["name"].(string)
				}
				expectedNames := []string{
					"aggregate",
					"get_attachment",
					"get_message",
					"get_stats",
					"list_messages",
					"search_by_domains",
					"search_in_message",
					"search_message_bodies",
					"search_messages",
					"search_metadata",
					"semantic_search_messages",
				}
				if shape.similar {
					expectedNames = append(expectedNames, "find_similar_messages")
				}
				if shape.document {
					expectedNames = append(expectedNames, ToolSearchDocuments)
				}
				if allowWrites {
					expectedNames = append(expectedNames, "export_attachment", "stage_deletion")
				}
				sort.Strings(expectedNames)
				checks.Equal(expectedNames, names)

				byName := toolsByName(t, tools)
				searchMessagesProperties := []string{"account", "limit", "offset", "query"}
				semanticProperties := []string{"query"}
				if shape.semantic {
					searchMessagesProperties = append(searchMessagesProperties, "explain", "min_score", "mode")
					semanticProperties = []string{"account", "explain", "limit", "min_score", "mode", "offset", "query"}
				}
				sort.Strings(searchMessagesProperties)
				checks.Equal(searchMessagesProperties, toolPropertyNames(t, byName[ToolSearchMessages]))
				checks.Equal(semanticProperties, toolPropertyNames(t, byName[ToolSemanticSearchMessages]))

				searchInMessageProperties := []string{"id", "limit", "offset", "query"}
				if shape.vectorInMessage {
					searchInMessageProperties = append(searchInMessageProperties, "min_score", "mode")
				}
				sort.Strings(searchInMessageProperties)
				checks.Equal(searchInMessageProperties, toolPropertyNames(t, byName[ToolSearchInMessage]))
				if shape.similar {
					checks.Equal(
						[]string{"account", "after", "before", "has_attachment", "limit", "message_id", "message_type"},
						toolPropertyNames(t, byName[ToolFindSimilarMessages]),
					)
				}
				if shape.document {
					checks.Equal(
						[]string{"attachment_id", "cursor", "limit", "message_id", "message_types", "query", "source_ids"},
						toolPropertyNames(t, byName[ToolSearchDocuments]),
					)
				}

				for _, tool := range tools {
					inputSchema, ok := tool["inputSchema"].(map[string]any)
					must.True(ok, "%s inputSchema", tool["name"])
					checks.Equal("https://json-schema.org/draft/2020-12/schema", inputSchema["$schema"], "%s input dialect", tool["name"])
					checks.Equal("object", inputSchema["type"], "%s input type", tool["name"])
					checks.Equal(false, inputSchema["additionalProperties"], "%s closed input", tool["name"])
					outputSchema, ok := tool["outputSchema"].(map[string]any)
					must.True(ok, "%s outputSchema", tool["name"])
					checks.Equal("https://json-schema.org/draft/2020-12/schema", outputSchema["$schema"], "%s output dialect", tool["name"])
					checks.Equal("object", outputSchema["type"], "%s output type", tool["name"])

					readOnly := tool["name"] != ToolExportAttachment && tool["name"] != ToolStageDeletion
					checks.Equal(map[string]any{
						"destructiveHint": false,
						"idempotentHint":  false,
						"openWorldHint":   false,
						"readOnlyHint":    readOnly,
					}, tool["annotations"], "%s annotations", tool["name"])
				}
			})
		}
	}

	checks := assert.New(t)
	tools := toolsByName(t, rawListTools(t, shapes[len(shapes)-1].opts, true))
	for _, field := range []struct {
		tool string
		name string
	}{
		{ToolGetMessage, "id"},
		{ToolGetAttachment, "attachment_id"},
		{ToolExportAttachment, "attachment_id"},
		{ToolFindSimilarMessages, "message_id"},
		{ToolSearchDocuments, "attachment_id"},
		{ToolSearchDocuments, "message_id"},
		{ToolListMessages, "conversation_id"},
		{ToolSearchInMessage, "id"},
	} {
		property := toolInputProperty(t, tools[field.tool], field.name)
		checks.Equal("integer", property["type"], "%s.%s type", field.tool, field.name)
		checks.InDelta(1, property["minimum"], 0, "%s.%s minimum", field.tool, field.name)
		checks.InDelta(jsonSafeIntegerMax, property["maximum"], 0, "%s.%s maximum", field.tool, field.name)
	}

	for _, toolName := range []string{
		ToolSearchMessages,
		ToolSearchMetadata,
		ToolSearchMessageBodies,
		ToolSemanticSearchMessages,
		ToolListMessages,
	} {
		limit := toolInputProperty(t, tools[toolName], "limit")
		checks.Equal("integer", limit["type"], "%s.limit type", toolName)
		checks.InDelta(0, limit["minimum"], 0, "%s.limit minimum", toolName)
		checks.InDelta(jsonSafeIntegerMax, limit["maximum"], 0, "%s.limit maximum", toolName)
		checks.InDelta(20, limit["default"], 0, "%s.limit default", toolName)
	}
	for toolName, defaultLimit := range map[string]float64{
		ToolFindSimilarMessages: 20,
		ToolSearchInMessage:     10,
		ToolAggregate:           50,
		ToolSearchByDomains:     100,
	} {
		limit := toolInputProperty(t, tools[toolName], "limit")
		checks.Equal("integer", limit["type"], "%s.limit type", toolName)
		checks.InDelta(0, limit["minimum"], 0, "%s.limit minimum", toolName)
		checks.InDelta(jsonSafeIntegerMax, limit["maximum"], 0, "%s.limit maximum", toolName)
		checks.InDelta(defaultLimit, limit["default"], 0, "%s.limit default", toolName)
	}
	for _, field := range []struct {
		tool string
		name string
	}{
		{ToolSearchMessages, "offset"},
		{ToolSearchMetadata, "offset"},
		{ToolSearchMessageBodies, "offset"},
		{ToolSemanticSearchMessages, "offset"},
		{ToolListMessages, "offset"},
		{ToolSearchByDomains, "offset"},
		{ToolSearchInMessage, "offset"},
		{ToolGetMessage, "offset"},
	} {
		property := toolInputProperty(t, tools[field.tool], field.name)
		checks.Equal("integer", property["type"], "%s.%s type", field.tool, field.name)
		checks.InDelta(0, property["minimum"], 0, "%s.%s minimum", field.tool, field.name)
		checks.InDelta(jsonSafeIntegerMax, property["maximum"], 0, "%s.%s maximum", field.tool, field.name)
		checks.InDelta(0, property["default"], 0, "%s.%s default", field.tool, field.name)
	}
	for _, name := range []string{"center_at", "max_chars"} {
		property := toolInputProperty(t, tools[ToolGetMessage], name)
		checks.Equal("integer", property["type"], name)
		checks.InDelta(-jsonSafeIntegerMax, property["minimum"], 0, name)
		checks.InDelta(jsonSafeIntegerMax, property["maximum"], 0, name)
	}
	checks.InDelta(-1, toolInputProperty(t, tools[ToolGetMessage], "center_at")["default"], 0)
	checks.InDelta(2000, toolInputProperty(t, tools[ToolGetMessage], "max_chars")["default"], 0)

	for _, field := range []struct {
		tool string
		name string
	}{
		{ToolSearchMessages, "min_score"},
		{ToolSemanticSearchMessages, "min_score"},
		{ToolSearchInMessage, "min_score"},
	} {
		property := toolInputProperty(t, tools[field.tool], field.name)
		checks.Equal("number", property["type"], "%s.%s type", field.tool, field.name)
		checks.InDelta(0, property["minimum"], 0, "%s.%s minimum", field.tool, field.name)
		checks.InDelta(1, property["maximum"], 0, "%s.%s maximum", field.tool, field.name)
		checks.InDelta(0, property["default"], 0, "%s.%s default", field.tool, field.name)
	}
	checks.Equal([]any{"vector", "hybrid"}, toolInputProperty(t, tools[ToolSearchMessages], "mode")["enum"])
	checks.Equal([]any{"vector", "hybrid"}, toolInputProperty(t, tools[ToolSemanticSearchMessages], "mode")["enum"])
	checks.Equal([]any{"keyword", "vector"}, toolInputProperty(t, tools[ToolSearchInMessage], "mode")["enum"])
	checks.Equal([]any{"auto", "text", "html"}, toolInputProperty(t, tools[ToolGetMessage], "body_format")["enum"])
	checks.Equal([]any{"sender", "recipient", "domain", "label", "time"}, toolInputProperty(t, tools[ToolAggregate], "group_by")["enum"])

	t.Run("invalid arguments do not invoke handlers", func(t *testing.T) {
		var calls atomic.Int64
		engine := &catalogCountingEngine{
			MockEngine: &querytest.MockEngine{},
			calls:      &calls,
		}
		countingHybrid := hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
			calls.Add(1)
			return &HybridSearchResult{}, nil
		})
		backend := &fakeBackend{}
		localHybrid := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{})
		opts := ServeOptions{
			Engine: engine, HybridSearcher: countingHybrid,
			HybridEngine: localHybrid, Backend: backend,
		}
		cases := []struct {
			name string
			tool string
			args map[string]any
		}{
			{name: "fractional integer", tool: ToolGetMessage, args: map[string]any{"id": 1.5}},
			{name: "unsafe ID", tool: ToolGetMessage, args: map[string]any{"id": 9007199254740992.0}},
			{name: "negative offset", tool: ToolListMessages, args: map[string]any{"offset": -1}},
			{name: "search messages score below zero", tool: ToolSearchMessages, args: map[string]any{"query": "test", "mode": "hybrid", "min_score": -0.1}},
			{name: "search messages score above one", tool: ToolSearchMessages, args: map[string]any{"query": "test", "mode": "hybrid", "min_score": 1.1}},
			{name: "semantic score below zero", tool: ToolSemanticSearchMessages, args: map[string]any{"query": "test", "min_score": -0.1}},
			{name: "semantic score above one", tool: ToolSemanticSearchMessages, args: map[string]any{"query": "test", "min_score": 1.1}},
			{name: "in-message score below zero", tool: ToolSearchInMessage, args: map[string]any{"id": 1, "query": "test", "mode": "vector", "min_score": -0.1}},
			{name: "in-message score above one", tool: ToolSearchInMessage, args: map[string]any{"id": 1, "query": "test", "mode": "vector", "min_score": 1.1}},
			{name: "unknown property", tool: ToolGetStats, args: map[string]any{"unexpected": true}},
			{name: "invalid enum", tool: ToolGetMessage, args: map[string]any{"id": 1, "body_format": "markdown"}},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				calls.Store(0)
				backend.activeCalls = 0
				result := rawCallTool(t, opts, test.tool, test.args)
				assert.Equal(t, true, result["isError"], "result: %#v", result)
				assert.Equal(t, int64(0), calls.Load())
				assert.Equal(t, 0, backend.activeCalls)
			})
		}
	})

	t.Run("valid numeric defaults and clamps", testNumericCompatibility)
}

type numericCaptureEngine struct {
	*querytest.MockEngine

	limit  int
	offset int
}

func newNumericCaptureEngine() *numericCaptureEngine {
	return &numericCaptureEngine{MockEngine: &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			1: {
				ID:          1,
				BodyText:    strings.Repeat("match ", 1200),
				From:        []query.Address{},
				To:          []query.Address{},
				Cc:          []query.Address{},
				Bcc:         []query.Address{},
				Labels:      []string{},
				Attachments: []query.AttachmentInfo{},
			},
		},
	}}
}

func (e *numericCaptureEngine) SearchFast(
	_ context.Context,
	_ *search.Query,
	_ query.MessageFilter,
	limit int,
	offset int,
) ([]query.MessageSummary, error) {
	e.limit, e.offset = limit, offset
	return []query.MessageSummary{}, nil
}

func (e *numericCaptureEngine) SearchMessageBodies(
	_ context.Context,
	_ *search.Query,
	limit int,
	offset int,
) ([]query.MessageSummary, error) {
	e.limit, e.offset = limit-1, offset // The handler requests one extra row for has_more.
	return []query.MessageSummary{}, nil
}

func (e *numericCaptureEngine) ListMessages(
	_ context.Context,
	filter query.MessageFilter,
) ([]query.MessageSummary, error) {
	e.limit, e.offset = filter.Pagination.Limit-1, filter.Pagination.Offset
	return []query.MessageSummary{}, nil
}

func (e *numericCaptureEngine) Aggregate(
	_ context.Context,
	_ query.ViewType,
	opts query.AggregateOptions,
) ([]query.AggregateRow, error) {
	e.limit = opts.Limit
	return []query.AggregateRow{}, nil
}

func (e *numericCaptureEngine) SearchByDomains(
	_ context.Context,
	_ []string,
	_, _ *time.Time,
	limit int,
	offset int,
) ([]query.MessageSummary, error) {
	e.limit, e.offset = limit, offset
	return []query.MessageSummary{}, nil
}

func testNumericCompatibility(t *testing.T) {
	engine := newNumericCaptureEngine()
	var hybridRequest HybridSearchRequest
	var similarRequest SimilarSearchRequest
	vectorCap := 7
	opts := ServeOptions{
		Engine: engine,
		HybridSearcher: hybridSearcherFunc(func(_ context.Context, req HybridSearchRequest) (*HybridSearchResult, error) {
			hybridRequest = req
			return &HybridSearchResult{Hits: []HybridSearchHit{}}, nil
		}),
		SimilarSearcher: similarSearcherFunc(func(_ context.Context, req SimilarSearchRequest) (*SimilarSearchResult, error) {
			similarRequest = req
			return &SimilarSearchResult{SeedMessageID: req.MessageID, Messages: []query.MessageSummary{}}, nil
		}),
		VectorCfg: vector.Config{Search: vector.SearchConfig{MaxPageSizeHybrid: &vectorCap}},
	}

	searchTools := []string{
		ToolSearchMessages,
		ToolSearchMetadata,
		ToolSearchMessageBodies,
		ToolSemanticSearchMessages,
		ToolListMessages,
	}
	variants := []struct {
		name       string
		arguments  map[string]any
		wantLimit  int
		wantOffset int
	}{
		{name: "omitted", arguments: map[string]any{}, wantLimit: 20},
		{name: "zero", arguments: map[string]any{"limit": 0}, wantLimit: 20},
		{name: "clamped", arguments: map[string]any{"limit": 5000, "offset": 5000}, wantLimit: 50, wantOffset: 1000},
	}
	for _, toolName := range searchTools {
		for _, variant := range variants {
			t.Run(toolName+"/"+variant.name, func(t *testing.T) {
				engine.limit, engine.offset = -1, -1
				hybridRequest = HybridSearchRequest{Limit: -1, Offset: -1}
				args := map[string]any{}
				maps.Copy(args, variant.arguments)
				if toolName != ToolListMessages {
					args["query"] = "match"
				}
				result := rawCallTool(t, opts, toolName, args)
				assert.NotEqual(t, true, result["isError"], "result: %#v", result)
				gotLimit, gotOffset := engine.limit, engine.offset
				if toolName == ToolSemanticSearchMessages {
					gotLimit, gotOffset = hybridRequest.Limit, hybridRequest.Offset
				}
				assert.Equal(t, variant.wantLimit, gotLimit)
				assert.Equal(t, variant.wantOffset, gotOffset)
			})
		}
	}

	for _, test := range []struct {
		name      string
		arguments map[string]any
		want      int
	}{
		{name: "omitted", arguments: map[string]any{"message_id": 1}, want: 7},
		{name: "zero", arguments: map[string]any{"message_id": 1, "limit": 0}, want: 7},
		{name: "clamped", arguments: map[string]any{"message_id": 1, "limit": 5000}, want: 7},
	} {
		t.Run(ToolFindSimilarMessages+"/"+test.name, func(t *testing.T) {
			result := rawCallTool(t, opts, ToolFindSimilarMessages, test.arguments)
			assert.NotEqual(t, true, result["isError"], "result: %#v", result)
			assert.Equal(t, test.want, similarRequest.Limit)
		})
	}
	t.Run(ToolFindSimilarMessages+"/global clamp without configured cap", func(t *testing.T) {
		noConfiguredCap := opts
		noConfiguredCap.VectorCfg = vector.Config{}
		result := rawCallTool(t, noConfiguredCap, ToolFindSimilarMessages, map[string]any{
			"message_id": 1,
			"limit":      5000,
		})
		assert.NotEqual(t, true, result["isError"], "result: %#v", result)
		assert.Equal(t, 1000, similarRequest.Limit)
	})

	for _, test := range []struct {
		name       string
		arguments  map[string]any
		wantReturn int
		wantOffset int
	}{
		{name: "omitted", arguments: map[string]any{}, wantReturn: 10},
		{name: "zero", arguments: map[string]any{"limit": 0}, wantReturn: 0},
		{name: "clamped", arguments: map[string]any{"limit": 5000}, wantReturn: 1000},
		{name: "offset clamped", arguments: map[string]any{"offset": 5000}, wantReturn: 10, wantOffset: 1000},
	} {
		t.Run(ToolSearchInMessage+"/"+test.name, func(t *testing.T) {
			args := map[string]any{"id": 1, "query": "match"}
			maps.Copy(args, test.arguments)
			result := rawCallTool(t, opts, ToolSearchInMessage, args)
			structured, ok := result["structuredContent"].(map[string]any)
			require.True(t, ok, "result: %#v", result)
			assert.InDelta(t, test.wantReturn, structured["returned"], 0)
			assert.InDelta(t, test.wantOffset, structured["offset"], 0)
		})
	}

	for _, test := range []struct {
		name string
		tool string
		args map[string]any
		want int
	}{
		{name: "aggregate omitted", tool: ToolAggregate, args: map[string]any{"group_by": "sender"}, want: 50},
		{name: "aggregate zero", tool: ToolAggregate, args: map[string]any{"group_by": "sender", "limit": 0}, want: 0},
		{name: "aggregate clamped", tool: ToolAggregate, args: map[string]any{"group_by": "sender", "limit": 5000}, want: 1000},
		{name: "domains omitted", tool: ToolSearchByDomains, args: map[string]any{"domains": "example.com"}, want: 100},
		{name: "domains zero", tool: ToolSearchByDomains, args: map[string]any{"domains": "example.com", "limit": 0}, want: 0},
		{name: "domains clamped", tool: ToolSearchByDomains, args: map[string]any{"domains": "example.com", "limit": 5000}, want: 1000},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine.limit = -1
			result := rawCallTool(t, opts, test.tool, test.args)
			assert.NotEqual(t, true, result["isError"], "result: %#v", result)
			assert.Equal(t, test.want, engine.limit)
		})
	}

	for _, test := range []struct {
		name       string
		arguments  map[string]any
		wantReturn int
		wantOffset int
	}{
		{name: "defaults", arguments: map[string]any{}, wantReturn: 2000},
		{name: "zero max chars", arguments: map[string]any{"max_chars": 0}, wantReturn: 2000},
		{name: "max chars clamped", arguments: map[string]any{"max_chars": 5000}, wantReturn: 4000},
		{name: "offset clamped to body", arguments: map[string]any{"offset": 9000}, wantOffset: 7200},
		{name: "negative center disables centering and uses offset", arguments: map[string]any{"center_at": -10, "offset": 123}, wantReturn: 2000, wantOffset: 123},
	} {
		t.Run(ToolGetMessage+"/"+test.name, func(t *testing.T) {
			args := map[string]any{"id": 1}
			maps.Copy(args, test.arguments)
			result := rawCallTool(t, opts, ToolGetMessage, args)
			structured, ok := result["structuredContent"].(map[string]any)
			require.True(t, ok, "result: %#v", result)
			assert.InDelta(t, test.wantReturn, structured["body_returned"], 0)
			assert.InDelta(t, test.wantOffset, structured["offset"], 0)
		})
	}

	for _, toolName := range []string{ToolSearchMessages, ToolSemanticSearchMessages} {
		for _, test := range []struct {
			name  string
			value any
			want  float64
		}{
			{name: "default", want: 0},
			{name: "provided", value: 0.75, want: 0.75},
		} {
			t.Run(toolName+"/min_score/"+test.name, func(t *testing.T) {
				hybridRequest.MinScore = -1
				args := map[string]any{"query": "match", "mode": "hybrid"}
				if test.value != nil {
					args["min_score"] = test.value
				}
				result := rawCallTool(t, opts, toolName, args)
				assert.NotEqual(t, true, result["isError"], "result: %#v", result)
				assert.InDelta(t, test.want, hybridRequest.MinScore, 0)
			})
		}
	}

	vectorCfg := testSimilarVectorConfig()
	vectorBackend := &fakeBackend{
		active: testSimilarActiveGeneration(vectorCfg),
		chunkHits: map[int64][]vector.ChunkHit{
			1: {
				{ChunkCharStart: 0, ChunkCharEnd: 3, Score: 0.25},
				{ChunkCharStart: 4, ChunkCharEnd: 8, Score: 0.9},
			},
		},
	}
	vectorEngine := hybrid.NewEngine(vectorBackend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: vectorCfg.GenerationFingerprint(), RRFK: 60, KPerSignal: 10,
	})
	vectorOpts := ServeOptions{
		Engine: &querytest.MockEngine{Messages: map[int64]*query.MessageDetail{
			1: {
				ID: 1, BodyText: "low high", From: []query.Address{}, To: []query.Address{},
				Cc: []query.Address{}, Bcc: []query.Address{}, Labels: []string{}, Attachments: []query.AttachmentInfo{},
			},
		}},
		HybridEngine: vectorEngine,
		VectorCfg:    vectorCfg,
		Backend:      vectorBackend,
	}
	for _, test := range []struct {
		name  string
		value any
		want  int
	}{
		{name: "default", want: 2},
		{name: "provided", value: 0.75, want: 1},
	} {
		t.Run(ToolSearchInMessage+"/min_score/"+test.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			args := map[string]any{"id": 1, "query": "semantic", "mode": "vector"}
			if test.value != nil {
				args["min_score"] = test.value
			}
			result := rawCallTool(t, vectorOpts, ToolSearchInMessage, args)
			checks.NotEqual(true, result["isError"], "result: %#v", result)
			structured, ok := result["structuredContent"].(map[string]any)
			must.True(ok, "result: %#v", result)
			data, ok := structured["data"].([]any)
			must.True(ok, "structured result: %#v", structured)
			checks.Len(data, test.want)
		})
	}
}

type catalogCountingEngine struct {
	*querytest.MockEngine

	calls *atomic.Int64
}

func (e *catalogCountingEngine) GetMessage(_ context.Context, id int64) (*query.MessageDetail, error) {
	e.calls.Add(1)
	return &query.MessageDetail{ID: id, BodyText: "test body"}, nil
}

func (e *catalogCountingEngine) ListMessages(_ context.Context, _ query.MessageFilter) ([]query.MessageSummary, error) {
	e.calls.Add(1)
	return nil, nil
}

func (e *catalogCountingEngine) GetTotalStats(_ context.Context, _ query.StatsOptions) (*query.TotalStats, error) {
	e.calls.Add(1)
	return &query.TotalStats{}, nil
}

func TestStructuredToolResult(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	stats := &query.TotalStats{
		MessageCount:    12,
		TotalSize:       3456,
		AttachmentCount: 7,
	}
	response := rawCallTool(t, ServeOptions{
		Engine: &querytest.MockEngine{
			Stats: stats,
			Accounts: []query.AccountInfo{
				{ID: 1, SourceType: "gmail", Identifier: "alice@example.com", DisplayName: "Test User"},
			},
		},
	}, ToolGetStats, map[string]any{})

	checks.Equal("complete", response["resultType"])
	structured, ok := response["structuredContent"].(map[string]any)
	must.True(ok, "result: %#v", response)
	content, ok := response["content"].([]any)
	must.True(ok)
	must.NotEmpty(content)
	textBlock, ok := content[0].(map[string]any)
	must.True(ok)
	text, ok := textBlock["text"].(string)
	must.True(ok)
	var textJSON map[string]any
	must.NoError(json.Unmarshal([]byte(text), &textJSON))
	checks.Equal(structured, textJSON)
	meta, ok := response["_meta"].(map[string]any)
	must.True(ok)
	checks.Equal(map[string]any{"name": "msgvault", "version": "1.0.0"}, meta["io.modelcontextprotocol/serverInfo"])
	statsResult, ok := structured["stats"].(map[string]any)
	must.True(ok)
	checks.InDelta(12, statsResult["MessageCount"], 0)
	accounts, ok := structured["accounts"].([]any)
	must.True(ok)
	must.NotEmpty(accounts)
	account, ok := accounts[0].(map[string]any)
	must.True(ok)
	checks.Equal("alice@example.com", account["Identifier"])
}

func TestArrayToolResultsUseObjectEnvelope(t *testing.T) {
	opts := ServeOptions{Engine: &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{{Key: "example.com", Count: 1}},
		SearchResults: []query.MessageSummary{{ID: 1, Subject: "Example"}},
	}}
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "aggregate", tool: ToolAggregate, args: map[string]any{"group_by": "domain"}},
		{name: "search by domains", tool: ToolSearchByDomains, args: map[string]any{"domains": "example.com"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := rawCallTool(t, opts, test.tool, test.args)
			structured, ok := result["structuredContent"].(map[string]any)
			require.True(t, ok, "result: %#v", result)
			data, ok := structured["data"].([]any)
			require.True(t, ok, "structured result: %#v", structured)
			assert.Len(t, data, 1)
		})
	}
}

func TestArrayToolEmptyResultsUseEmptyDataArray(t *testing.T) {
	opts := ServeOptions{Engine: &querytest.MockEngine{}}
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "aggregate", tool: ToolAggregate, args: map[string]any{"group_by": "domain"}},
		{name: "search by domains", tool: ToolSearchByDomains, args: map[string]any{"domains": "example.com"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := rawCallTool(t, opts, test.tool, test.args)
			assert.Equal(t, map[string]any{"data": []any{}}, result["structuredContent"])
		})
	}
}

func TestOfficialSDKInMemoryRoundTrip(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{
		Engine: &querytest.MockEngine{
			Stats:    &query.TotalStats{MessageCount: 3},
			Accounts: []query.AccountInfo{},
		},
	}, true).Connect(ctx, serverTransport, nil)
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(serverSession.Close()) })

	client := sdkmcp.NewClient(
		&sdkmcp.Implementation{Name: "msgvault-test", Version: "1.0.0"},
		&sdkmcp.ClientOptions{Capabilities: &sdkmcp.ClientCapabilities{}},
	)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(clientSession.Close()) })

	listed, err := clientSession.ListTools(ctx, nil)
	must.NoError(err)
	names := make([]string, len(listed.Tools))
	for i, tool := range listed.Tools {
		names[i] = tool.Name
	}
	checks.Contains(names, ToolGetStats)
	checks.Contains(names, ToolExportAttachment)

	result, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      ToolGetStats,
		Arguments: map[string]any{},
	})
	must.NoError(err)
	checks.False(result.IsError)
	structured, ok := result.StructuredContent.(map[string]any)
	must.True(ok, "structured content: %#v", result.StructuredContent)
	statsResult, ok := structured["stats"].(map[string]any)
	must.True(ok)
	checks.InDelta(3, statsResult["MessageCount"], 0)
	must.NotEmpty(result.Content)
	textContent, ok := result.Content[0].(*sdkmcp.TextContent)
	must.True(ok, "content: %#v", result.Content)
	var fallback map[string]any
	must.NoError(json.Unmarshal([]byte(textContent.Text), &fallback))
	checks.Equal(structured, fallback)
}
