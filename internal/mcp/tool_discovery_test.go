package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func searchToolsResponse(t *testing.T, opts ServeOptions, writes bool, query string, limit int) toolDiscoveryResponse {
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
	return result
}

func TestToolDiscoveryColdStart(t *testing.T) {
	assertions := assert.New(t)
	must := require.New(t)

	tools := toolsByName(t, rawListTools(t, ServeOptions{}, false))
	must.Contains(tools, ToolSearchTools)
	annotations, ok := tools[ToolSearchTools]["annotations"].(map[string]any)
	must.True(ok)
	assertions.Equal(true, annotations["readOnlyHint"])
	response := searchToolsResponse(t, ServeOptions{}, false, "read one message", 3)
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
}

func TestToolDiscoveryUsefulQueriesAndMetadataParity(t *testing.T) {
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: &savedViewServiceFixture{}, ArchiveSQLQuerier: &sqlToolEngine{}}
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

			response := searchToolsResponse(t, opts, false, tc.query, 5)
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
					case nil:
						assertions.Empty(input.Types)
					default:
						must.Fail("unexpected schema type", "%T", types)
					}
					assertions.Equal(slices.Contains(required, any(input.Name)), input.Required)
				}
			}
			assertions.Contains(names, tc.want)
		})
	}
}

func TestToolDiscoveryVisibilityMatchesToolsList(t *testing.T) {
	calendar := &calendarMCPFake{}
	opts := ServeOptions{
		Engine: &querytest.MockEngine{}, SavedViews: &savedViewServiceFixture{},
		DirectoryBackend: &directoryPeopleBackend{}, ArchiveSQLQuerier: &sqlToolEngine{},
		Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_source_scheduler_status", "sync_source"},
		OperationWriteFamilies: []OperationFamily{OperationFamilySources},
		Calendar:               calendar, AllowCalendarWrites: true,
		Drafts: &draftTestRunner{}, DraftCommands: []string{"draft-compose", "draft-get", "draft-delete"},
	}
	delegated := opts
	delegated.DelegatedOnly, delegated.GrantPermissions = true, []string{"message.read"}
	modes := []struct {
		name   string
		opts   ServeOptions
		writes bool
	}{
		{"owner read only", opts, false}, {"owner writes", opts, true},
		{"delegated message only", delegated, true}, {"delegated no backend or grants", ServeOptions{DelegatedOnly: true}, true},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			assertions := assert.New(t)

			listed := toolsByName(t, rawListTools(t, mode.opts, mode.writes))
			require.Contains(t, listed, ToolSearchTools)
			for _, query := range []string{"calendar", "source scheduler sync", "draft compose delete", "sql query", "saved view delete", "get message", "directory people", "export attachment"} {
				response := searchToolsResponse(t, mode.opts, mode.writes, query, 20)
				for _, result := range response.Results {
					assertions.Contains(listed, result.Name, "%s leaked %s", query, result.Name)
				}
			}
			// Every advertised operation is discoverable by its exact tool name.
			for name := range listed {
				response := searchToolsResponse(t, mode.opts, mode.writes, name, 0)
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

	calendar := &calendarMCPFake{}
	probes := 0
	families := &ArchiveFamilies{}
	opts := ServeOptions{Calendar: calendar, AllowCalendarWrites: true,
		DiscoverFamilies: func(context.Context) *ArchiveFamilies { probes++; return families },
	}
	for _, present := range []bool{false, true, false} {
		families.Calendar = present
		listed := toolsByName(t, rawListTools(t, opts, true))
		assertions.Equal(present, listed["calendar_create"] != nil)
		before := probes
		response := searchToolsResponse(t, opts, true, "calendar", 20)
		assertions.Equal(before+1, probes)
		found := false
		for _, result := range response.Results {
			found = found || result.Name == "calendar_create"
		}
		assertions.Equal(present, found)
	}
	// A broader dynamic snapshot cannot override the server's static family filter.
	opts.Families = &ArchiveFamilies{}
	families.Calendar = true
	response := searchToolsResponse(t, opts, true, "calendar", 20)
	for _, result := range response.Results {
		assertions.NotContains(result.Name, "calendar_")
	}
}

func TestToolDiscoveryInputLimits(t *testing.T) {
	assertions := assert.New(t)

	client := task5ConnectClient(t, ServeOptions{}, false)
	cases := []map[string]any{
		{"query": ""}, {"query": " \t\n"}, {"query": strings.Repeat("a", 513)}, {"query": strings.Repeat("界", 171)},
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
	for _, query := range []string{strings.Repeat("a", 512), strings.Repeat("界", 170) + "ab"} {
		response := searchToolsResponse(t, ServeOptions{}, false, query, 1)
		assertions.Equal(query, response.Query)
	}
	response := searchToolsResponse(t, ServeOptions{}, false, "message", 0)
	assertions.Len(response.Results, 5)
}

func TestToolDiscoveryFullMetadataAndStableOrdering(t *testing.T) {
	assertions := assert.New(t)
	must := require.New(t)
	definitions := make([]toolDefinition, 0, 21)
	for i := range 21 {
		inputs := map[string]*jsonschema.Schema{}
		for j := range 4 {
			inputs[fmt.Sprintf("input_%02d", j)] = stringSchema("description")
		}
		definitions = append(definitions, readDefinition(fmt.Sprintf("fixture_%02d", i), "fixture description", closedObject(inputs, "input_03"), outputSchemaFor[toolDiscoveryResponse](), nil))
	}
	result, err := discoverTools(definitions, "fixture", 20)
	must.NoError(err)
	var response toolDiscoveryResponse
	must.NoError(json.Unmarshal(result.structuredContent, &response))
	must.Len(response.Results, 20)
	assertions.True(response.Truncated)
	for i, item := range response.Results {
		assertions.Equal(fmt.Sprintf("fixture_%02d", i), item.Name)
		assertions.Equal("fixture description", item.Description)
		must.Len(item.Inputs, 4)
		assertions.Equal("input_00", item.Inputs[0].Name)
		assertions.Equal("description", item.Inputs[0].Description)
		assertions.True(item.Inputs[3].Required)
	}
	slices.Reverse(definitions)
	again, err := discoverTools(definitions, "fixture", 20)
	must.NoError(err)
	assertions.JSONEq(string(result.structuredContent), string(again.structuredContent))
	for _, tc := range []struct {
		query        string
		limit, count int
		truncated    bool
	}{
		{"zzzzzzunmatched", 20, 0, false}, {"fixture", 1, 1, true},
	} {
		result, err := discoverTools(definitions, tc.query, tc.limit)
		must.NoError(err)
		var response toolDiscoveryResponse
		must.NoError(json.Unmarshal(result.structuredContent, &response))
		assertions.NotNil(response.Results)
		assertions.Len(response.Results, tc.count)
		assertions.Equal(tc.truncated, response.Truncated)
	}
}

func TestToolDiscoveryUnionTypes(t *testing.T) {
	assertions := assert.New(t)
	backend := &namedPromotionTestBackend{recordingPeopleBackend: &recordingPeopleBackend{}}
	response := searchToolsResponse(t, peopleToolOptions(backend), true, ToolPromotePerson, 20)
	index := slices.IndexFunc(response.Results, func(result toolDiscoveryResult) bool { return result.Name == ToolPromotePerson })
	require.NotEqual(t, -1, index)
	inputs := make(map[string]toolDiscoveryInput)
	for _, input := range response.Results[index].Inputs {
		inputs[input.Name] = input
	}
	assertions.Equal([]string{"null", "string"}, inputs["display_name"].Types)
	assertions.False(inputs["display_name"].Required)
	assertions.Equal([]string{"integer"}, inputs["participant_id"].Types)
}

func TestToolDiscoveryLegacyProtocolCatalog(t *testing.T) {
	must := require.New(t)
	opts := ServeOptions{Calendar: &calendarMCPFake{}, AllowCalendarWrites: true,
		Operations: operationalTestBackend{}, OperationCapabilities: []string{"sync_source"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources},
	}
	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	listRecorder, listed := task5LegacyHTTPPost(t, handler, "2025-11-25", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	task3RequireSuccess(t, listRecorder, listed)
	names := task3ToolNames(t, listed)
	callRecorder, called := task5LegacyHTTPPost(t, handler, "2025-11-25", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_tools","arguments":{"query":"calendar source sync","limit":20}}}`)
	task3RequireSuccess(t, callRecorder, called)
	must.NotEqual(true, called.Result["isError"])
	var response toolDiscoveryResponse
	encoded, err := json.Marshal(toolStructuredContent(t, called.Result))
	must.NoError(err)
	must.NoError(json.Unmarshal(encoded, &response))
	must.NotEmpty(response.Results)
	for _, result := range response.Results {
		assert.Contains(t, names, result.Name)
	}
}

func TestToolDiscoveryStdioCatalogSnapshot(t *testing.T) {
	initial := false
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

func TestToolDiscoveryDirectNamesAtDefaultLimit(t *testing.T) {
	opts := ServeOptions{SavedViews: &savedViewServiceFixture{}}
	listed := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, listed, ToolListSavedViews)
	for _, query := range []string{"list saved views", ToolListSavedViews} {
		response := searchToolsResponse(t, opts, true, query, 0)
		names := make([]string, 0, len(response.Results))
		for _, result := range response.Results {
			names = append(names, result.Name)
		}
		assert.Contains(t, names, ToolListSavedViews, "query %q", query)
	}
}
