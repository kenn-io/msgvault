package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
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
	backend, imported, _ := newMeetingMCPBackend(t)
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
	backend := &meetingBackendFixture{}
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

type meetingBackendCall struct {
	tool, ref, decision string
	messageID           int64
}

type meetingBackendFixture struct {
	calls []meetingBackendCall
	err   error
}

func (*meetingBackendFixture) GetMeetingContext(context.Context, generated.GetMeetingContextBody) (*meetingcontent.PacketResult, error) {
	return &meetingcontent.PacketResult{}, nil
}

func (*meetingBackendFixture) ListMeetingActionItems(context.Context, generated.ListMeetingActionItemsBody) (*meetingcontent.ActionsPage, error) {
	return &meetingcontent.ActionsPage{}, nil
}

func (*meetingBackendFixture) GetMeetingMetrics(context.Context, generated.GetMeetingMetricsBody) (*meetingcontent.Metrics, error) {
	return &meetingcontent.Metrics{}, nil
}

func (f *meetingBackendFixture) MeetingRecord(_ context.Context, ref string) (store.MeetingRecord, error) {
	f.calls = append(f.calls, meetingBackendCall{tool: ToolGetMeetingRecord, ref: ref})
	return store.MeetingRecord{Links: []store.MeetingLink{}, MessageIDs: []int64{301}}, f.err
}
func (f *meetingBackendFixture) SuggestMeetingLinks(_ context.Context, ref string) (store.MeetingSuggestionPage, error) {
	f.calls = append(f.calls, meetingBackendCall{tool: ToolSuggestMeetingLinks, ref: ref})
	return store.MeetingSuggestionPage{Suggestions: []store.MeetingSuggestion{{Ref: "evt:302", MemberMessageID: 301, OverlapMinutes: 17, SharedAttendees: 2, TitleTokens: 1, Reasons: []string{"fixture reason"}}}, Unexpandable: []store.CalendarUnexpandable{}}, f.err
}
func (f *meetingBackendFixture) DecideMeetingLink(_ context.Context, id int64, ref, decision string) error {
	tool := ToolLinkMeeting
	if decision == "unlink" {
		tool = ToolUnlinkMeeting
	}
	f.calls = append(f.calls, meetingBackendCall{tool: tool, ref: ref, decision: decision, messageID: id})
	return f.err
}

func TestMeetingLinkToolsRoundTrip(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	backend := &meetingBackendFixture{}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, Meetings: backend, MeetingLinks: true}, true).Connect(ctx, serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "meeting-links-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(session.Close()) })
	for _, tc := range []struct {
		tool string
		args map[string]any
		call meetingBackendCall
		want map[string]any
	}{
		{ToolSuggestMeetingLinks, map[string]any{"ref": "msg:73"}, meetingBackendCall{tool: ToolSuggestMeetingLinks, ref: "msg:73"}, map[string]any{"suggestions": []any{map[string]any{"ref": "evt:302", "member_message_id": float64(301), "overlap_minutes": float64(17), "shared_attendees": float64(2), "title_tokens": float64(1), "reasons": []any{"fixture reason"}}}, "unexpandable": []any{}}},
		{ToolGetMeetingRecord, map[string]any{"ref": "evt:91@20270314T100000Z"}, meetingBackendCall{tool: ToolGetMeetingRecord, ref: "evt:91@20270314T100000Z"}, map[string]any{"links": []any{}, "message_ids": []any{float64(301)}}},
		{ToolLinkMeeting, map[string]any{"message_id": float64(73), "event": "evt:91"}, meetingBackendCall{tool: ToolLinkMeeting, ref: "evt:91", decision: "link", messageID: 73}, map[string]any{"message_id": float64(73), "event": "evt:91", "decision": "link"}},
		{ToolUnlinkMeeting, map[string]any{"message_id": float64(73), "event": "evt:91"}, meetingBackendCall{tool: ToolUnlinkMeeting, ref: "evt:91", decision: "unlink", messageID: 73}, map[string]any{"message_id": float64(73), "event": "evt:91", "decision": "unlink"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			backend.calls = nil
			result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			require.NoError(err)
			require.False(result.IsError)
			assert.Equal([]meetingBackendCall{tc.call}, backend.calls)
			assert.Equal(tc.want, meetingStructuredContent(t, result))
		})
	}
}

func TestMeetingLinkToolsReportDaemonClientErrors(t *testing.T) {
	backend := &meetingBackendFixture{}
	ctx := t.Context()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, Meetings: backend, MeetingLinks: true}, true).Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "meeting-errors-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		code       string
	}{
		{"get record 400", ToolGetMeetingRecord, map[string]any{"ref": "msg:73"}, "invalid_argument"},
		{"get record 404", ToolGetMeetingRecord, map[string]any{"ref": "msg:99999999"}, "meeting_not_found"},
		{"suggest links 400", ToolSuggestMeetingLinks, map[string]any{"ref": "msg:73"}, "invalid_argument"},
		{"link meeting 400", ToolLinkMeeting, map[string]any{"message_id": float64(73), "event": "evt:91"}, "invalid_argument"},
		{"unlink meeting 400", ToolUnlinkMeeting, map[string]any{"message_id": float64(73), "event": "evt:91@20270314T100000Z"}, "invalid_argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			status := 400
			if tc.code == "meeting_not_found" {
				status = 404
			}
			backend.err = &daemonclient.APIError{Status: status, Code: tc.code, Message: "fixture daemon refusal"}
			result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			require.NoError(err)
			require.True(result.IsError)
			require.NotEmpty(result.Content)
			content, ok := result.Content[0].(*sdkmcp.TextContent)
			require.True(ok)
			assert.Contains(t, content.Text, tc.code)
		})
	}
}
