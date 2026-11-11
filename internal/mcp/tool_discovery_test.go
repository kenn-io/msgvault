package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func searchToolsResponse(t *testing.T, opts ServeOptions, writes bool, query string, limit int) (toolDiscoveryResponse, map[string]any) {
	t.Helper()
	args := map[string]any{"query": query}
	if limit != 0 {
		args["limit"] = limit
	}
	response := rawModernCall(t, opts, HTTPOptions{AllowWrites: writes}, "tools/call", map[string]any{"name": ToolSearchTools, "arguments": args})
	require.NotEqual(t, true, response.Result["isError"], "%#v", response.Result)
	content := toolStructuredContent(t, response.Result)
	encoded, err := json.Marshal(content)
	require.NoError(t, err)
	var result toolDiscoveryResponse
	require.NoError(t, json.Unmarshal(encoded, &result))
	return result, response.Result
}

func TestToolDiscoveryColdStart(t *testing.T) {
	assertions := assert.New(t)
	must := require.New(t)

	tools := toolsByName(t, rawListTools(t, ServeOptions{}, false))
	must.Contains(tools, ToolSearchTools)
	annotations, ok := tools[ToolSearchTools]["annotations"].(map[string]any)
	must.True(ok)
	assertions.Equal(true, annotations["readOnlyHint"])
	response, _ := searchToolsResponse(t, ServeOptions{}, false, "read one message", 3)
	must.NotEmpty(response.Results)
	assertions.Equal(ToolGetMessage, response.Results[0].Name)
	var id *toolDiscoveryInput
	for i := range response.Results[0].Inputs {
		if response.Results[0].Inputs[i].Name == "id" {
			id = &response.Results[0].Inputs[i]
		}
	}
	must.NotNil(id)
	assertions.Equal([]string{"integer"}, id.Types)
	assertions.True(id.Required)
	assertions.True(response.Results[0].ReadOnly)
}

func TestToolDiscoveryUsefulQueriesAndMetadataParity(t *testing.T) {
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: &savedViewServiceFixture{}, ArchiveSQLQuerier: &sqlToolEngine{}}
	// The backend is only capability metadata. No query or provider method should run.
	cases := []struct{ query, want string }{
		{"read one message", ToolGetMessage},
		{"list conversation thread", ToolListThread},
		{"attachment filename mime_families", ToolSearchPersonFiles},
		{"group statistics top senders", ToolAggregate},
		{"run saved view", ToolRunSavedView},
		{"query sql", ToolQuerySQL},
	}
	listed := toolsByName(t, rawListTools(t, opts, false))
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			assertions := assert.New(t)
			must := require.New(t)

			response, firstWire := searchToolsResponse(t, opts, false, tc.query, 5)
			again, secondWire := searchToolsResponse(t, opts, false, tc.query, 5)
			assertions.Equal(response, again)
			assertions.Equal(firstWire, secondWire)
			names := make([]string, 0, len(response.Results))
			for _, result := range response.Results {
				names = append(names, result.Name)
				must.Contains(listed, result.Name)
				properties := toolPropertyNames(t, listed[result.Name])
				schema, ok := listed[result.Name]["inputSchema"].(map[string]any)
				must.True(ok)
				required, _ := schema["required"].([]any)
				for _, input := range result.Inputs {
					assertions.Contains(properties, input.Name)
					actual := toolInputProperty(t, listed[result.Name], input.Name)
					switch types := actual["type"].(type) {
					case string:
						assertions.Equal([]string{types}, input.Types)
					case []any:
						assertions.ElementsMatch(types, input.Types)
					}
					assertions.Equal(slices.Contains(required, any(input.Name)), input.Required)
				}
			}
			assertions.Contains(names, tc.want)
		})
	}
}

func TestToolDiscoveryVisibilityMatchesToolsList(t *testing.T) {
	calendar := calendarServiceBackend(func(context.Context, calcontrol.Request) (*calcontrol.Result, error) {
		require.FailNow(t, "discovery must not execute calendar operations")
		return nil, assert.AnError
	})
	opts := ServeOptions{
		Engine: &querytest.MockEngine{}, SavedViews: &savedViewServiceFixture{},
		DirectoryBackend: &directoryPeopleBackend{}, ArchiveSQLQuerier: &sqlToolEngine{},
		Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_source_scheduler_status", "sync_source"},
		OperationWriteFamilies: []OperationFamily{OperationFamilySources},
		Calendar:               calendar, AllowCalendarWrites: true,
		Drafts: &draftTestRunner{}, DraftCommands: []string{"draft-compose", "draft-get", "draft-delete"},
	}
	modes := []struct {
		name   string
		opts   ServeOptions
		writes bool
	}{
		{"owner read only", opts, false}, {"owner writes", opts, true},
	}
	delegated := opts
	delegated.DelegatedOnly, delegated.GrantPermissions = true, []string{"message.read"}
	modes = append(modes, struct {
		name   string
		opts   ServeOptions
		writes bool
	}{"delegated message only", delegated, true})
	noGrants := ServeOptions{DelegatedOnly: true}
	modes = append(modes, struct {
		name   string
		opts   ServeOptions
		writes bool
	}{"delegated no backend or grants", noGrants, true})
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			assertions := assert.New(t)

			listed := toolsByName(t, rawListTools(t, mode.opts, mode.writes))
			require.Contains(t, listed, ToolSearchTools)
			if mode.name == "delegated no backend or grants" {
				assertions.Len(listed, 1)
			}
			for _, query := range []string{"calendar", "source scheduler sync", "draft compose delete", "sql query", "saved view delete", "get message", "directory people", "export attachment"} {
				response, _ := searchToolsResponse(t, mode.opts, mode.writes, query, 20)
				for _, result := range response.Results {
					assertions.Contains(listed, result.Name, "%s leaked %s", query, result.Name)
				}
			}
			// Every advertised operation is discoverable by its exact tool name.
			for name := range listed {
				response, _ := searchToolsResponse(t, mode.opts, mode.writes, name, 20)
				names := make([]string, 0, len(response.Results))
				for _, result := range response.Results {
					names = append(names, result.Name)
				}
				assertions.Contains(names, name)
			}
		})
	}
}

func TestToolDiscoveryAppliesFreshArchiveFamilies(t *testing.T) {
	assertions := assert.New(t)

	calendar := calendarServiceBackend(func(context.Context, calcontrol.Request) (*calcontrol.Result, error) {
		require.FailNow(t, "metadata discovery must not execute calendar operations")
		return nil, assert.AnError
	})
	probes := 0
	families := &ArchiveFamilies{}
	opts := ServeOptions{Calendar: calendar, AllowCalendarWrites: true,
		DiscoverFamilies: func(context.Context) *ArchiveFamilies { probes++; return families },
	}
	for _, present := range []bool{false, true, false} {
		families.Calendar = present
		listed := toolsByName(t, rawListTools(t, opts, true))
		assertions.Equal(present, listed["calendar_create"] != nil)
		response, _ := searchToolsResponse(t, opts, true, "calendar", 20)
		found := false
		for _, result := range response.Results {
			assertions.Contains(listed, result.Name)
			found = found || result.Name == "calendar_create"
		}
		assertions.Equal(present, found)
	}
	assertions.Equal(6, probes)
	// A broader dynamic snapshot cannot override the server's static family filter.
	opts.Families = &ArchiveFamilies{}
	families.Calendar = true
	response, _ := searchToolsResponse(t, opts, true, "calendar", 20)
	for _, result := range response.Results {
		assertions.NotContains(result.Name, "calendar_")
	}
}

func TestToolDiscoveryInputLimits(t *testing.T) {
	assertions := assert.New(t)

	client := task5ConnectClient(t, ServeOptions{}, false)
	cases := []map[string]any{
		{}, {"query": ""}, {"query": " \t\n"}, {"query": 1},
		{"query": strings.Repeat("a", 513)}, {"query": strings.Repeat("界", 171)},
		{"query": "message", "limit": 0}, {"query": "message", "limit": -1},
		{"query": "message", "limit": 21}, {"query": "message", "limit": 1.5},
		{"query": "message", "limit": "5"}, {"query": "message", "extra": true},
	}
	for i, args := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			assertions := assert.New(t)

			result, err := client.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchTools, Arguments: args})
			require.NoError(t, err)
			assertions.True(result.IsError)
		})
	}
	// Byte boundaries and Unicode survive the transport without truncating input.
	for _, query := range []string{strings.Repeat("a", 512), strings.Repeat("界", 170) + "ab", "🌲 café 日本語"} {
		response, _ := searchToolsResponse(t, ServeOptions{}, false, query, 1)
		assertions.Equal(query, response.Query)
	}
	response, _ := searchToolsResponse(t, ServeOptions{}, false, "zzzzzzunmatched", 0)
	assertions.Empty(response.Results)
	assertions.NotNil(response.Results)
	assertions.False(response.Truncated)
	response, _ = searchToolsResponse(t, ServeOptions{}, false, "message", 1)
	assertions.Len(response.Results, 1)
	assertions.True(response.Truncated)
	response, _ = searchToolsResponse(t, ServeOptions{}, false, "message", 0)
	assertions.LessOrEqual(len(response.Results), 5)
}

func TestToolDiscoveryFullMetadataAndStableOrdering(t *testing.T) {
	assertions := assert.New(t)
	must := require.New(t)
	definitions := make([]toolDefinition, 0, 21)
	for i := range 21 {
		inputs := map[string]*jsonschema.Schema{}
		for j := range 40 {
			inputs[fmt.Sprintf("input_%02d", j)] = stringSchema(strings.Repeat("description ", 30))
		}
		definitions = append(definitions, readDefinition(fmt.Sprintf("fixture_%02d", i), strings.Repeat("fixture ", 50), closedObject(inputs, "input_39"), outputSchemaFor[toolDiscoveryResponse](), nil))
	}
	result, err := discoverTools(definitions, "fixture", 20)
	must.NoError(err)
	var response toolDiscoveryResponse
	must.NoError(json.Unmarshal(result.structuredContent, &response))
	must.Len(response.Results, 20)
	assertions.True(response.Truncated)
	for i, item := range response.Results {
		assertions.Equal(fmt.Sprintf("fixture_%02d", i), item.Name)
		assertions.Equal(strings.Repeat("fixture ", 50), item.Description)
		must.Len(item.Inputs, 40)
		assertions.Equal("input_00", item.Inputs[0].Name)
		assertions.Equal(strings.Repeat("description ", 30), item.Inputs[0].Description)
		assertions.True(item.Inputs[39].Required)
	}
	slices.Reverse(definitions)
	again, err := discoverTools(definitions, "fixture", 20)
	must.NoError(err)
	assertions.JSONEq(string(result.structuredContent), string(again.structuredContent))
}

func TestToolDiscoveryUnionTypes(t *testing.T) {
	assertions := assert.New(t)
	must := require.New(t)
	backend := &namedPromotionTestBackend{recordingPeopleBackend: &recordingPeopleBackend{}}
	opts := peopleToolOptions(backend)
	listed := toolsByName(t, rawListTools(t, opts, true))
	response, _ := searchToolsResponse(t, opts, true, ToolPromotePerson, 20)
	for _, result := range response.Results {
		if result.Name != ToolPromotePerson {
			continue
		}
		inputs := make(map[string]toolDiscoveryInput)
		for _, input := range result.Inputs {
			inputs[input.Name] = input
		}
		assertions.Equal([]string{"null", "string"}, inputs["display_name"].Types)
		assertions.False(inputs["display_name"].Required)
		assertions.ElementsMatch([]string{"null", "string"}, toolInputProperty(t, listed[ToolPromotePerson], "display_name")["type"])
		assertions.Equal([]string{"integer"}, inputs["participant_id"].Types)
		assertions.Zero(backend.namedParticipantID)
		assertions.Zero(backend.promoteParticipantID)
		return
	}
	must.Fail("promote_person missing from discovery")
}

func TestToolDiscoveryHTTPProtocolAndBearerPolicy(t *testing.T) {
	calendar := calendarServiceBackend(func(context.Context, calcontrol.Request) (*calcontrol.Result, error) {
		require.FailNow(t, "metadata discovery must not execute calendar operations")
		return nil, assert.AnError
	})
	opts := ServeOptions{Calendar: calendar, AllowCalendarWrites: true,
		Operations: operationalTestBackend{}, OperationCapabilities: []string{"sync_source"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources},
	}
	for _, protocol := range []string{"2025-11-25", modernProtocolVersion} {
		t.Run(protocol, func(t *testing.T) {
			assertions := assert.New(t)
			must := require.New(t)

			handler, closeHandler := NewHTTPHandler(opts, HTTPOptions{APIKey: "synthetic-key", AllowWrites: true})
			t.Cleanup(closeHandler)
			call := func(method string, args map[string]any, authenticated bool) *httptest.ResponseRecorder {
				t.Helper()
				if args == nil {
					args = map[string]any{}
				}
				meta := modernRequestMeta()
				meta["io.modelcontextprotocol/protocolVersion"] = protocol
				args["_meta"] = meta
				payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": args})
				must.NoError(err)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("Mcp-Protocol-Version", protocol)
				req.Header.Set("Mcp-Method", method)
				if name, ok := args["name"].(string); ok {
					req.Header.Set("Mcp-Name", name)
				}
				if authenticated {
					req.Header.Set("Authorization", "Bearer synthetic-key")
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, req)
				return recorder
			}
			assertions.Equal(http.StatusUnauthorized, call("tools/list", nil, false).Code)
			listed := call("tools/list", nil, true)
			must.Equal(http.StatusOK, listed.Code, listed.Body.String())
			var list rawRPCResponse
			must.NoError(json.Unmarshal(listed.Body.Bytes(), &list))
			must.Empty(list.Error)
			entries, ok := list.Result["tools"].([]any)
			must.True(ok)
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				tool, ok := entry.(map[string]any)
				must.True(ok)
				name, ok := tool["name"].(string)
				must.True(ok)
				names = append(names, name)
			}
			assertions.Contains(names, ToolSearchTools)
			assertions.Equal(protocol == modernProtocolVersion, slices.Contains(names, "calendar_create"))
			assertions.Equal(protocol == modernProtocolVersion, slices.Contains(names, "sync_source"))
			args := map[string]any{"name": ToolSearchTools, "arguments": map[string]any{"query": "calendar source sync", "limit": 20}}
			assertions.Equal(http.StatusUnauthorized, call("tools/call", args, false).Code)
			result := call("tools/call", args, true)
			must.Equal(http.StatusOK, result.Code, result.Body.String())
			var response rawRPCResponse
			must.NoError(json.Unmarshal(result.Body.Bytes(), &response))
			must.Empty(response.Error)
			must.NotEqual(true, response.Result["isError"])
			var found toolDiscoveryResponse
			encoded, err := json.Marshal(toolStructuredContent(t, response.Result))
			must.NoError(err)
			must.NoError(json.Unmarshal(encoded, &found))
			must.NotEmpty(found.Results)
			for _, tool := range found.Results {
				assertions.Contains(names, tool.Name)
			}
		})
	}
}

func TestToolDiscoveryStdioCatalogSnapshot(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(strconv.FormatBool(initial), func(t *testing.T) {
			assertions := assert.New(t)
			must := require.New(t)
			families := &ArchiveFamilies{Calendar: initial}
			opts := ServeOptions{Calendar: &calendarMCPFake{}, AllowCalendarWrites: true, Families: &ArchiveFamilies{Calendar: initial}, DiscoverFamilies: func(context.Context) *ArchiveFamilies { return families }}
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(opts, true).Connect(t.Context(), serverTransport, nil)
			must.NoError(err)
			t.Cleanup(func() { assertions.NoError(server.Close()) })
			client, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "snapshot", Version: "1"}, nil).Connect(t.Context(), clientTransport, nil)
			must.NoError(err)
			t.Cleanup(func() { assertions.NoError(client.Close()) })
			families.Calendar = !initial
			listed, err := client.ListTools(t.Context(), nil)
			must.NoError(err)
			names := make([]string, 0, len(listed.Tools))
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			assertions.Equal(initial, slices.Contains(names, "calendar_create"))
			wire, err := client.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchTools, Arguments: map[string]any{"query": "calendar_create", "limit": 20}})
			must.NoError(err)
			must.False(wire.IsError)
			response := task5StructuredAs[toolDiscoveryResponse](t, wire)
			names = names[:0]
			for _, result := range response.Results {
				names = append(names, result.Name)
			}
			assertions.Equal(initial, slices.Contains(names, "calendar_create"))
		})
	}
}

func TestToolDiscoveryServeTransportSnapshotsOnce(t *testing.T) {
	assertions := assert.New(t)
	must := require.New(t)
	probes := 0
	families := &ArchiveFamilies{Calendar: true}
	opts := ServeOptions{Calendar: &calendarMCPFake{}, AllowCalendarWrites: true, DiscoverFamilies: func(context.Context) *ArchiveFamilies { probes++; return families }}
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ServeTransport(ctx, opts, serverTransport) }()
	t.Cleanup(func() { cancel(); <-done })
	client, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "snapshot", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	must.NoError(err)
	families = &ArchiveFamilies{}
	listed, err := client.ListTools(ctx, nil)
	must.NoError(err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assertions.Contains(names, "calendar_create")
	wire, err := client.CallTool(ctx, &sdkmcp.CallToolParams{Name: ToolSearchTools, Arguments: map[string]any{"query": "calendar_create", "limit": 20}})
	must.NoError(err)
	must.False(wire.IsError)
	response := task5StructuredAs[toolDiscoveryResponse](t, wire)
	names = nil
	for _, result := range response.Results {
		names = append(names, result.Name)
	}
	assertions.Contains(names, "calendar_create")
	assertions.Equal(1, probes)
	assertions.NoError(client.Close())
}
