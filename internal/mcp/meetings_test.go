package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newMeetingMCPBackend(t *testing.T) (*daemonclient.Client, []meetingimport.Result, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	importer := meetingimport.NewImporter(st, meetingimport.Hooks{})
	actions := []meetingimport.MeetingActionItem{
		{SourceID: "draft", Title: "Send MCP draft", AssigneeEmail: "alice@example.com", Status: "open"},
	}
	unrelatedActions := []meetingimport.MeetingActionItem{
		{SourceID: "outside", Title: "Send outside MCP draft", AssigneeEmail: "alice@example.com", Status: "open"},
	}
	requests := []meetingimport.Request{
		{
			Source: meetingimport.Source{Identifier: "meeting-mcp-fixture", AccountEmail: "owner@example.com"},
			Meeting: meetingimport.Meeting{
				ExternalID: "first", Title: "MCP planning", StartedAt: "2026-05-01T10:00:00Z",
				EndedAt: "2026-05-01T10:20:00Z", SummaryText: "Prepared the MCP launch.",
				Transcript: "Alice: send the MCP draft.", ActionItems: &actions,
				Organizer: &meetingimport.MeetingPerson{Name: "Owner", Email: "owner@example.com"},
				Attendees: []meetingimport.MeetingPerson{{Name: "Alice", Email: "alice@example.com"}},
			},
		},
		{
			Source: meetingimport.Source{Identifier: "meeting-mcp-unrelated", AccountEmail: "owner@other.example"},
			Meeting: meetingimport.Meeting{
				ExternalID: "second", Title: "MCP follow-up", StartedAt: "2026-06-01T10:00:00Z",
				SummaryText: "Checked the MCP launch.",
				Organizer:   &meetingimport.MeetingPerson{Name: "Other Owner", Email: "owner@other.example"},
				Attendees:   []meetingimport.MeetingPerson{{Name: "Bob", Email: "bob@other.example"}},
				ActionItems: &unrelatedActions,
			},
		},
	}
	results := make([]meetingimport.Result, len(requests))
	for i, request := range requests {
		var err error
		results[i], err = importer.Import(t.Context(), request)
		require.NoError(t, err)
	}
	server := httptest.NewServer(api.NewServer(
		&config.Config{}, st, nil, slog.New(slog.DiscardHandler),
	).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	return client, results, st
}

func TestMeetingToolsRoundTripThroughOfficialSDKAndStoreBackedDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	backend, imported, st := newMeetingMCPBackend(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{
		Engine: &querytest.MockEngine{}, Meetings: backend,
	}, false).Connect(ctx, serverTransport, nil)
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(
		&sdkmcp.Implementation{Name: "meeting-test", Version: "1.0.0"},
		&sdkmcp.ClientOptions{Capabilities: &sdkmcp.ClientCapabilities{}},
	)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(clientSession.Close()) })

	listed, err := clientSession.ListTools(ctx, nil)
	must.NoError(err)
	tools := make(map[string]*sdkmcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{ToolGetMeetingContext, ToolListMeetingActionItems, ToolGetMeetingMetrics} {
		must.Contains(tools, name)
		checks.True(tools[name].Annotations.ReadOnlyHint)
	}

	contextResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: ToolGetMeetingContext,
		Arguments: map[string]any{
			"message_ids": []any{float64(imported[0].MessageID)},
			"format":      "markdown", "include_transcript": true,
		},
	})
	must.NoError(err)
	checks.False(contextResult.IsError)
	contextStructured := meetingStructuredContent(t, contextResult)
	checks.Equal("markdown", contextStructured["format"])
	checks.Contains(contextStructured["content"], "MCP planning")
	checks.Contains(contextStructured["content"], "Alice: send the MCP draft.")

	allActionResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: ToolListMeetingActionItems,
		Arguments: map[string]any{
			"assignee_email": "alice@example.com", "status": "pending",
		},
	})
	must.NoError(err)
	checks.False(allActionResult.IsError)
	allActionStructured := meetingStructuredContent(t, allActionResult)
	allRows, ok := allActionStructured["rows"].([]any)
	must.True(ok)
	must.Len(allRows, 2)

	actionResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: ToolListMeetingActionItems,
		Arguments: map[string]any{
			"domains": []any{"example.com"}, "assignee_email": "alice@example.com", "status": "pending",
		},
	})
	must.NoError(err)
	checks.False(actionResult.IsError)
	actionStructured := meetingStructuredContent(t, actionResult)
	rows, ok := actionStructured["rows"].([]any)
	must.True(ok)
	must.Len(rows, 1)
	row, ok := rows[0].(map[string]any)
	must.True(ok)
	action, ok := row["action"].(map[string]any)
	must.True(ok)
	checks.Equal("Send MCP draft", action["title"])
	checks.NotEqual(imported[0].SourceID, imported[1].SourceID)

	allMetricsResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: ToolGetMeetingMetrics, Arguments: map[string]any{},
	})
	must.NoError(err)
	checks.False(allMetricsResult.IsError)
	allMetricsStructured := meetingStructuredContent(t, allMetricsResult)
	allTotals, ok := allMetricsStructured["totals"].(map[string]any)
	must.True(ok)
	checks.InDelta(2, allTotals["meeting_count"], 0)
	checks.InDelta(1, allTotals["known_duration_count"], 0)
	checks.InDelta(1, allTotals["unknown_duration_count"], 0)

	metricsResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      ToolGetMeetingMetrics,
		Arguments: map[string]any{"source_ids": []any{float64(imported[0].SourceID)}},
	})
	must.NoError(err)
	checks.False(metricsResult.IsError)
	metricsStructured := meetingStructuredContent(t, metricsResult)
	totals, ok := metricsStructured["totals"].(map[string]any)
	must.True(ok)
	checks.InDelta(1, totals["meeting_count"], 0)
	checks.InDelta(1, totals["known_duration_count"], 0)
	checks.InDelta(0, totals["unknown_duration_count"], 0)
	checks.InDelta(1200, totals["average_known_seconds"], 0)

	invalidTimestampResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      ToolGetMeetingMetrics,
		Arguments: map[string]any{"after": "2026-05-01"},
	})
	must.NoError(err)
	checks.True(invalidTimestampResult.IsError)

	boundedMetricsResult, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: ToolGetMeetingMetrics,
		Arguments: map[string]any{
			"after": "2026-05-01T12:30:00+02:00",
		},
	})
	must.NoError(err)
	checks.False(boundedMetricsResult.IsError)
	boundedMetrics := meetingStructuredContent(t, boundedMetricsResult)
	boundedTotals, ok := boundedMetrics["totals"].(map[string]any)
	must.True(ok)
	checks.InDelta(1, boundedTotals["meeting_count"], 0)
	checks.InDelta(0, boundedTotals["known_duration_count"], 0)
	checks.InDelta(1, boundedTotals["unknown_duration_count"], 0)
	checks.Nil(boundedTotals["average_known_seconds"])

	emptyMetrics, err := backend.GetMeetingMetrics(ctx, generated.GetMeetingMetricsBody{
		Scope: &generated.MeetingScopeRequest{MessageIds: &[]int64{}},
	})
	must.NoError(err)
	must.NotNil(emptyMetrics)
	checks.Nil(emptyMetrics.Totals.AverageKnownSeconds)
	source, err := st.GetOrCreateSource("twenty", "owner@example.test")
	must.NoError(err)
	raw := []byte(`{"schema_version":1,"calendar_event":{"startsAt":"2025-01-01T00:00:00Z"},"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"participant":{"name":"A"},"words":[{"text":"Hello","start_timestamp":{"absolute":"2026-01-01T00:00:10Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:12Z"}}]}]}}`)
	snapshot := meetingarchive.Snapshot{SourceID: source.ID, AccountEmail: "owner@example.test", SourceMessageID: "subtitle", SourceConversationID: "subtitle", Title: "Meeting", StartedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Raw: raw, RawFormat: "twenty_json", Organizer: &meetingarchive.Person{Name: "Owner", Email: "owner@example.test"}}
	archived, err := meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	must.NoError(err)
	subtitles, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{Name: ToolGetMeetingContext, Arguments: map[string]any{"message_ids": []any{float64(archived.MessageID)}, "format": "vtt"}})
	must.NoError(err)
	must.False(subtitles.IsError)
	checks.Equal("WEBVTT\n\n1\n00:00:10.000 --> 00:00:12.000\nA: Hello\n\n", meetingStructuredContent(t, subtitles)["content"])
	snapshot.Raw = []byte(strings.Replace(string(raw), `"startedAt":"2026-01-01T00:00:00Z",`, "", 1))
	_, err = meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	must.NoError(err)
	invalid, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{Name: ToolGetMeetingContext, Arguments: map[string]any{"message_ids": []any{float64(archived.MessageID)}, "format": "vtt"}})
	must.NoError(err)
	must.True(invalid.IsError)
	must.NotEmpty(invalid.Content)
	message, ok := invalid.Content[0].(*sdkmcp.TextContent)
	must.True(ok)
	checks.Contains(message.Text, "invalid_meeting_subtitles")
	snapshot.Raw = []byte(strings.Replace(string(raw), `"Hello"`, `"`+strings.Repeat("x", 5000)+`"`, 1))
	_, err = meetingarchive.New(st).Upsert(t.Context(), snapshot, meetingarchive.UpsertOptions{})
	must.NoError(err)
	budget, err := clientSession.CallTool(ctx, &sdkmcp.CallToolParams{Name: ToolGetMeetingContext, Arguments: map[string]any{"message_ids": []any{float64(archived.MessageID)}, "format": "vtt", "max_bytes": 4096}})
	must.NoError(err)
	must.True(budget.IsError)
	must.NotEmpty(budget.Content)
	message, ok = budget.Content[0].(*sdkmcp.TextContent)
	must.True(ok)
	checks.Contains(message.Text, "context_budget_too_small")
	checks.Contains(message.Text, "max_bytes")
	checks.Contains(message.Text, "1048576")

}

func meetingStructuredContent(t *testing.T, result *sdkmcp.CallToolResult) map[string]any {
	t.Helper()
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok, "structured content: %#v", result.StructuredContent)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	require.True(t, ok, "content: %#v", result.Content)
	var fallback map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &fallback))
	assert.Equal(t, structured, fallback)
	return structured
}

func TestMeetingCatalogUsesClosedTypedSchemasAndStablePointers(t *testing.T) {
	assertions := assert.New(t)
	backend := meetingBackendFixture{}
	first := operationCatalog(ServeOptions{Engine: &querytest.MockEngine{}, Meetings: backend}, &handlers{})
	second := operationCatalog(ServeOptions{Engine: &querytest.MockEngine{}, Meetings: backend}, &handlers{})
	byName := make(map[string]toolDefinition, len(first))
	for i, definition := range first {
		byName[definition.name] = definition
		assertions.Same(definition.inputSchema, second[i].inputSchema)
		assertions.Same(definition.outputSchema, second[i].outputSchema)
	}
	for _, name := range []string{ToolGetMeetingContext, ToolListMeetingActionItems, ToolGetMeetingMetrics} {
		definition, ok := byName[name]
		require.True(t, ok, name)
		assertions.Equal("object", definition.inputSchema.Type)
		assertions.NotNil(definition.inputSchema.AdditionalProperties)
		assertions.Equal("object", definition.outputSchema.Type)
		assertions.True(definition.annotations.ReadOnlyHint)
	}
	for _, name := range []string{ToolListMeetingActionItems, ToolGetMeetingMetrics} {
		for _, argument := range []string{toolArgAfter, toolArgBefore} {
			assertions.Equal("date-time", byName[name].inputSchema.Properties[argument].Format)
		}
	}
	without := operationCatalog(ServeOptions{Engine: &querytest.MockEngine{}}, &handlers{})
	for _, definition := range without {
		assertions.NotContains([]string{ToolGetMeetingContext, ToolListMeetingActionItems, ToolGetMeetingMetrics}, definition.name)
	}
}

type meetingBackendFixture struct{}

func (meetingBackendFixture) GetMeetingContext(context.Context, generated.GetMeetingContextBody) (*meetingcontent.PacketResult, error) {
	return &meetingcontent.PacketResult{}, nil
}

func (meetingBackendFixture) ListMeetingActionItems(context.Context, generated.ListMeetingActionItemsBody) (*meetingcontent.ActionsPage, error) {
	return &meetingcontent.ActionsPage{}, nil
}

func (meetingBackendFixture) GetMeetingMetrics(context.Context, generated.GetMeetingMetricsBody) (*meetingcontent.Metrics, error) {
	return &meetingcontent.Metrics{}, nil
}
