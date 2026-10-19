package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

// The fake is the daemon transport boundary: these tests exercise real MCP
// schemas, dispatch, disclosure, confirmation and response projection.
type loopMCPBackend struct {
	pages        map[string]loops.ListResponse
	listed       []loops.ListOptions
	waiting      loops.WaitingOptions
	gotID        int64
	report       freshness.Report
	suppressions []loops.Suppression
	calls        []loopMCPWrite
	readErr      error
}

type loopMCPErrorBackend struct {
	*loopMCPBackend

	err error
}

func (b loopMCPErrorBackend) ListOpenLoops(context.Context, loops.ListOptions) (loops.ListResponse, error) {
	return loops.ListResponse{}, b.err
}

func (b loopMCPErrorBackend) GetOpenLoop(context.Context, int64) (loops.DetailResponse, error) {
	return loops.DetailResponse{}, b.err
}

func (b loopMCPErrorBackend) CloseOpenLoop(context.Context, int64, string) (loops.DetailResponse, error) {
	return loops.DetailResponse{}, b.err
}

type loopMCPWrite struct {
	action      string
	id          int64
	note        string
	until       time.Time
	suppression loops.SuppressionRequest
}

func (b *loopMCPBackend) ListOpenLoops(_ context.Context, opts loops.ListOptions) (loops.ListResponse, error) {
	b.listed = append(b.listed, opts)
	if b.readErr != nil {
		return loops.ListResponse{}, b.readErr
	}
	if b.pages != nil {
		return b.pages[opts.Cursor], nil
	}
	return loops.ListResponse{Items: []loops.Loop{{ID: 9, Kind: loops.ReplyOwed, Summary: "Synthetic archived subject", Evidence: []loops.Evidence{}}}, Freshness: b.report}, nil
}
func (b *loopMCPBackend) GetOpenLoop(_ context.Context, id int64) (loops.DetailResponse, error) {
	b.gotID = id
	return loops.DetailResponse{Loop: loops.Loop{ID: 9, Summary: "Synthetic archived subject", Evidence: []loops.Evidence{{Subject: "Archived evidence"}}}, Freshness: b.report}, nil
}
func (b *loopMCPBackend) GetWaitingLoops(_ context.Context, opts loops.WaitingOptions) (loops.WaitingResponse, error) {
	b.waiting = opts
	if b.report.Stale && !opts.AllowStale {
		return loops.WaitingResponse{}, &loops.StaleError{Response: loops.LoopsStaleResponse{Error: "loops_stale", Message: "Archive is stale", Freshness: b.report}}
	}
	return loops.WaitingResponse{Count: 1, Groups: []loops.WaitingGroup{{LoopCount: 1, Loops: []loops.Loop{{ID: 9}}}}, Freshness: b.report}, nil
}
func (b *loopMCPBackend) CloseOpenLoop(_ context.Context, id int64, note string) (loops.DetailResponse, error) {
	b.calls = append(b.calls, loopMCPWrite{action: "close", id: id, note: note})
	return loops.DetailResponse{Loop: loops.Loop{ID: id, Status: "closed"}, Freshness: b.report}, nil
}
func (b *loopMCPBackend) SnoozeOpenLoop(_ context.Context, id int64, until time.Time) (loops.DetailResponse, error) {
	b.calls = append(b.calls, loopMCPWrite{action: "snooze", id: id, until: until})
	return loops.DetailResponse{Loop: loops.Loop{ID: id, SnoozedUntil: &until}, Freshness: b.report}, nil
}
func (b *loopMCPBackend) ListLoopSuppressions(context.Context) ([]loops.Suppression, error) {
	return b.suppressions, nil
}
func (b *loopMCPBackend) CreateLoopSuppression(_ context.Context, r loops.SuppressionRequest) (loops.SuppressionResult, error) {
	b.calls = append(b.calls, loopMCPWrite{action: "mute", suppression: r})
	return loops.SuppressionResult{WouldClose: 3}, nil
}
func (b *loopMCPBackend) DeleteLoopSuppression(_ context.Context, id int64) error {
	b.calls = append(b.calls, loopMCPWrite{action: "unmute", id: id})
	return nil
}
func loopMCPOptions(b *loopMCPBackend) ServeOptions {
	return ServeOptions{Engine: &querytest.MockEngine{}, Loops: b, AllowLoopWrites: true}
}

func TestOpenLoopsCatalogRequiresBackendAndWritePolicies(t *testing.T) {
	for _, backend := range []bool{false, true} {
		for _, optIn := range []bool{false, true} {
			for _, writes := range []bool{false, true} {
				t.Run(fmt.Sprintf("backend=%t/optin=%t/writes=%t", backend, optIn, writes), func(t *testing.T) {
					opts := ServeOptions{Engine: &querytest.MockEngine{}, AllowLoopWrites: optIn}
					if backend {
						opts.Loops = &loopMCPBackend{}
					}
					listed := toolsByName(t, rawListTools(t, opts, writes))
					for _, name := range []string{"list_open_loops", "get_open_loop"} {
						assert.Equal(t, backend, listed[name] != nil)
					}
					for _, name := range []string{"close_open_loop", "snooze_open_loop", "mute_open_loops", "unmute_open_loops"} {
						assert.Equal(t, backend && optIn && writes, listed[name] != nil)
					}
				})
			}
		}
	}
	delegated := loopMCPOptions(&loopMCPBackend{})
	delegated.DelegatedOnly = true
	listed := toolsByName(t, rawListTools(t, delegated, true))
	assert.NotContains(t, listed, "list_open_loops")
	assert.NotContains(t, listed, "close_open_loop")
}
func TestOpenLoopsReadViewsPreserveFreshnessAndFilters(t *testing.T) {
	for _, view := range []string{"open", "held", "closed"} {
		t.Run(view, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			b := &loopMCPBackend{report: freshness.Report{Stale: true, StaleReasons: []string{"open_loops_missing"}}}
			result := rawCallTool(t, loopMCPOptions(b), "list_open_loops", map[string]any{"view": view, "kind": "reply_owed", "person_id": float64(7), "limit": float64(13)})
			requirements.NotEqual(true, result["isError"], "%v", result)
			body := toolStructuredContent(t, result)
			assertions.Equal("ok", body["status"])
			assertions.Len(body["items"], 1)
			currency, ok := body["freshness"].(map[string]any)
			requirements.True(ok)
			assertions.Equal(true, currency["stale"])
			requirements.Len(b.listed, 1)
			assertions.Equal(loops.ListOptions{Status: view, Kind: "reply_owed", PersonID: 7, Limit: 13}, b.listed[0])
		})
	}
	b := &loopMCPBackend{report: freshness.Report{Stale: true, StaleReasons: []string{"open_loops_missing"}}}
	detail := rawCallTool(t, loopMCPOptions(b), "get_open_loop", map[string]any{"id": float64(9)})
	detailLoop, ok := toolStructuredContent(t, detail)["loop"].(map[string]any)
	assertions := assert.New(t)
	requirements := require.New(t)
	requirements.True(ok)
	assertions.Equal("Synthetic archived subject", detailLoop["summary"])
	assertions.Equal(int64(9), b.gotID)
}
func TestOpenLoopsWaitingStaleIsStructuralAndOverrideExplicit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := &loopMCPBackend{report: freshness.Report{Stale: true, StaleReasons: []string{"open_loops_missing"}}}
	opts := loopMCPOptions(b)
	result := rawCallTool(t, opts, "list_open_loops", map[string]any{"view": "waiting"})
	requirements.NotEqual(true, result["isError"])
	body := toolStructuredContent(t, result)
	assertions.Equal("stale", body["status"])
	assertions.NotContains(body, "items")
	assertions.NotContains(body, "groups")
	assertions.NotEmpty(body["next_action"])
	currency, ok := body["freshness"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(true, currency["stale"])
	result = rawCallTool(t, opts, "list_open_loops", map[string]any{"view": "waiting", "allow_stale": true, "limit": float64(3)})
	body = toolStructuredContent(t, result)
	assertions.Equal("ok", body["status"])
	assertions.Len(body["groups"], 1)
	assertions.Equal(loops.WaitingOptions{Top: 3, AllowStale: true}, b.waiting)
	for _, args := range []map[string]any{{"view": "waiting", "kind": "reply_owed"}, {"view": "waiting", "person_id": float64(7)}} {
		result = rawCallTool(t, opts, "list_open_loops", args)
		assertions.Equal(true, result["isError"])
		assertions.Contains(toolErrorTextFromResult(t, result), "view=open")
	}
}
func TestOpenLoopsPersonProfileCountsAllPagesAndRetainsFive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	report := freshness.Report{Stale: true, Projections: map[string]*freshness.ProjectionFreshness{"open_loops": {Stale: true}}}
	kinds := []string{"reply_owed", "awaiting_reply", "commitment_owed_by_me", "commitment_owed_to_me"}
	rows := make([]loops.Loop, 201)
	for i := range rows {
		rows[i] = loops.Loop{ID: int64(i + 1), Kind: kinds[i%len(kinds)]}
	}
	b := &loopMCPBackend{pages: map[string]loops.ListResponse{"": {Items: rows[:200], NextCursor: "second", Freshness: report}, "second": {Items: rows[200:], Freshness: report}}}
	people := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{Person: store.Person{ID: 7, CreatedAt: now}}}
	opts := peopleToolOptions(people)
	opts.Loops = b
	result := rawCallTool(t, opts, "get_person_profile", map[string]any{"person_id": float64(7)})
	body := toolStructuredContent(t, result)
	block, ok := body["open_loops"].(map[string]any)
	requirements.True(ok, "%v", body)
	assertions.InDelta(float64(201), block["count"], 0)
	assertions.InDelta(float64(101), block["waiting_on_me"], 0)
	assertions.InDelta(float64(100), block["waiting_on_them"], 0)
	items, ok := block["items"].([]any)
	requirements.True(ok)
	requirements.Len(items, 5)
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		requirements.True(ok)
		assertions.InDelta(float64(i+1), item["id"], 0)
	}
	currency, ok := block["freshness"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(true, currency["stale"])
	requirements.Len(b.listed, 2)
	for i, o := range b.listed {
		assertions.Equal(int64(7), o.PersonID)
		assertions.Equal("open", o.Status)
		assertions.Equal(200, o.Limit)
		if i == 0 {
			assertions.Empty(o.Cursor)
		} else {
			assertions.Equal("second", o.Cursor)
		}
	}
	opts.Loops = nil
	result = rawCallTool(t, opts, "get_person_profile", map[string]any{"person_id": float64(7)})
	assertions.Nil(toolStructuredContent(t, result)["open_loops"])
}
func TestOpenLoopsPersonProfileSurvivesUnavailableProjection(t *testing.T) {
	assertions := assert.New(t)
	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	displayName := "Example person"
	people := &profileReadingPeopleBackend{profile: &peoplebrowser.PersonProfile{Person: store.Person{ID: 7, DisplayName: &displayName}}}
	opts := peopleToolOptions(people)
	opts.Loops = &loopMCPBackend{readErr: errors.New("synthetic projection unavailable")}
	result := rawCallTool(t, opts, "get_person_profile", map[string]any{"person_id": float64(7)})
	assertions.NotEqual(true, result["isError"])
	body := toolStructuredContent(t, result)
	assertions.InDelta(7, body["person_id"], 0)
	assertions.Equal("Example person", body["display_name"])
	assertions.Nil(body["open_loops"], "unavailable must never look like a certified empty projection")
	assertions.Equal("Open loops are temporarily unavailable.", body["open_loops_warning"])
	assertions.Contains(logs.String(), "synthetic projection unavailable")
	assertions.Contains(logs.String(), "person_id=7")
}

func TestOpenLoopsWritesConfirmCompleteDisclosureAndUseDurableSuppressionID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   map[string]any
		action string
	}{
		{"close_open_loop", map[string]any{"id": float64(9), "note": "Owner checked"}, "close"},
		{"snooze_open_loop", map[string]any{"id": float64(9), "until": "1h"}, "snooze"},
		{"mute_open_loops", map[string]any{"target_kind": "person", "id": float64(7), "note": "Owner checked", "dry_run": true}, "mute"},
		{"unmute_open_loops", map[string]any{"target_kind": "person", "id": float64(7)}, "unmute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			b := &loopMCPBackend{suppressions: []loops.Suppression{{ID: 99, TargetKind: "person", PersonID: new(int64(7))}}}
			result := confirmedCallTool(t, loopMCPOptions(b), tc.name, tc.args, true, func(message string) {
				assert.Contains(t, message, fmt.Sprint(tc.args["id"]))
				if note, ok := tc.args["note"]; ok {
					assert.Contains(t, message, note)
				}
			})
			requirements.NotEqual(true, result["isError"], "%v", result)
			requirements.Len(b.calls, 1)
			assertions.Equal(tc.action, b.calls[0].action)
			switch tc.action {
			case "unmute":
				assertions.Equal(int64(99), b.calls[0].id)
			case "mute":
				assertions.Equal(loops.SuppressionRequest{TargetKind: "person", PersonID: new(int64(7)), Note: "Owner checked", DryRun: true}, b.calls[0].suppression)
			case "close":
				assertions.Equal(int64(9), b.calls[0].id)
				assertions.Equal("Owner checked", b.calls[0].note)
			case "snooze":
				assertions.Equal(int64(9), b.calls[0].id)
				assertions.True(b.calls[0].until.After(time.Now()))
			}
		})
	}
}

func TestOpenLoopDaemonErrorsBecomeSafeToolErrors(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		args   map[string]any
		code   string
		status int
		want   string
	}{
		{
			name: "not found", tool: ToolGetOpenLoop, args: map[string]any{"id": float64(9)},
			code: "loop_not_found", status: 404, want: "loop_not_found: open loop was not found",
		},
		{
			name: "already closed", tool: ToolCloseOpenLoop,
			args: map[string]any{"id": float64(9), "note": "Owner checked"},
			code: "loop_already_closed", status: 409, want: "loop_already_closed: open loop is already closed",
		},
		{
			name: "disabled", tool: ToolListOpenLoops, args: map[string]any{"view": "open"},
			code: "loops_disabled", status: 503, want: "loops_disabled: open loops are unavailable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			backend := loopMCPErrorBackend{
				loopMCPBackend: &loopMCPBackend{},
				err:            &daemonclient.APIError{Status: tc.status, Code: tc.code, Message: "private daemon error"},
			}
			opts := loopMCPOptions(&loopMCPBackend{})
			opts.Loops = backend
			result := confirmedCallTool(t, opts, tc.tool, tc.args, true)

			requirements.Equal(true, result["isError"])
			content, ok := result["content"].([]any)
			requirements.True(ok, "%v", result)
			requirements.Len(content, 1)
			item, ok := content[0].(map[string]any)
			requirements.True(ok, "%v", content[0])
			message, ok := item["text"].(string)
			requirements.True(ok, "%v", item["text"])
			assertions.Equal(tc.want, message)
			assertions.NotContains(message, "private daemon error")
		})
	}
}

func TestOpenLoopsWritesFailClosedOnDeclineOrUnavailableConfirmation(t *testing.T) {
	for _, name := range []string{"close_open_loop", "snooze_open_loop", "mute_open_loops", "unmute_open_loops"} {
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			b := &loopMCPBackend{suppressions: []loops.Suppression{{ID: 99, TargetKind: "person", PersonID: new(int64(7))}}}
			args := map[string]any{"id": float64(7)}
			if name == "snooze_open_loop" {
				args["until"] = "1h"
			}
			if name == "mute_open_loops" || name == "unmute_open_loops" {
				args["target_kind"] = "person"
			}
			result := confirmedCallTool(t, loopMCPOptions(b), name, args, false)
			assertions.Equal(true, result["isError"])
			assertions.Empty(b.calls)
			unavailable, err := loopDefinitionByName(t, name).bind(&handlers{loops: b})(t.Context(), toolRequest{arguments: args})
			requirements.NoError(err)
			requirements.NotNil(unavailable)
			assertions.True(unavailable.isError)
			assertions.Contains(unavailable.text, "confirmation is unavailable")
			assertions.Empty(b.calls)
		})
	}
}
func TestOpenLoopsChangedArgumentsCannotReuseConfirmation(t *testing.T) {
	cases := []struct {
		name, key string
		value     any
		args      map[string]any
	}{
		{"close_open_loop", "id", float64(10), map[string]any{"id": float64(9)}},
		{"close_open_loop", "note", "Changed", map[string]any{"id": float64(9), "note": "Original"}},
		{"snooze_open_loop", "until", "2h", map[string]any{"id": float64(9), "until": "1h"}},
		{"mute_open_loops", "target_kind", "participant", map[string]any{"id": float64(7), "target_kind": "person"}},
		{"mute_open_loops", "dry_run", false, map[string]any{"id": float64(7), "target_kind": "person", "dry_run": true}},
		{"mute_open_loops", "note", "Changed", map[string]any{"id": float64(7), "target_kind": "person", "note": "Original"}},
		{"unmute_open_loops", "id", float64(8), map[string]any{"id": float64(7), "target_kind": "person"}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.key, func(t *testing.T) {
			assertions := assert.New(t)
			b := &loopMCPBackend{suppressions: []loops.Suppression{{ID: 99, TargetKind: "person", PersonID: new(int64(7))}, {ID: 100, TargetKind: "person", PersonID: new(int64(8))}}}
			result := confirmedCallTool(t, loopMCPOptions(b), tc.name, tc.args, true, func(string) { tc.args[tc.key] = tc.value })
			assertions.Equal(true, result["isError"])
			assertions.Empty(b.calls)
		})
	}
}

func loopDefinitionByName(t *testing.T, name string) toolDefinition {
	t.Helper()
	for _, d := range loopDefinitions() {
		if d.name == name {
			return d
		}
	}
	require.FailNow(t, "missing definition", name)
	return toolDefinition{}
}
func connectLoopMCPClient(t *testing.T, server *sdkmcp.Server, client *sdkmcp.Client) *sdkmcp.ClientSession {
	t.Helper()
	ct, st := sdkmcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ss.Close()) })
	cs, err := client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cs.Close()) })
	return cs
}
func TestOpenLoopsSnoozeRetainsDisclosedInstantAcrossDelayedModernApproval(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	now := time.Now().UTC()
	reference := now
	manager := newConfirmationChallenges()
	manager.now = func() time.Time { return now }
	b := &loopMCPBackend{}
	h := &handlers{loops: b}
	definition := loopDefinitionByName(t, "snooze_open_loop")
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "loop-clock-test", Version: "1"}, nil)
	sdkmcp.AddTool[map[string]any, any](server, definition.tool(), officialToolHandler(definition.bind(h), confirmationConfig{manager: manager}))
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "client", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session := connectLoopMCPClient(t, server, client)
	args := map[string]any{"id": float64(9), "until": "1h"}
	first, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "snooze_open_loop", Arguments: args})
	requirements.NoError(err)
	requirements.True(first.NeedsInput())
	prompt, ok := first.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	requirements.True(ok)
	assertions.Contains(prompt.Message, reference.Add(time.Hour).Format(time.RFC3339Nano))
	now = now.Add(2 * time.Minute)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "snooze_open_loop", Arguments: args, RequestState: first.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	requirements.False(result.IsError, "%v", result.Content)
	requirements.Len(b.calls, 1)
	assertions.Equal(reference.Add(time.Hour), b.calls[0].until)
	replay, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "snooze_open_loop", Arguments: args, RequestState: first.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	assertions.True(replay.IsError)
	assertions.Len(b.calls, 1)
	encoded, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(encoded), reference.Add(time.Hour).Format(time.RFC3339Nano))
}

func TestOpenLoopsSnoozeExpiresDuringConfirmation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	reference := time.Now().UTC().Add(-2 * time.Minute)
	manager := newConfirmationChallenges()
	manager.now = func() time.Time { return reference }
	b := &loopMCPBackend{}
	h := &handlers{loops: b}
	definition := loopDefinitionByName(t, "snooze_open_loop")
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "loop-clock-test", Version: "1"}, nil)
	sdkmcp.AddTool[map[string]any, any](server, definition.tool(), officialToolHandler(definition.bind(h), confirmationConfig{manager: manager}))
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "client", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session := connectLoopMCPClient(t, server, client)
	args := map[string]any{"id": float64(9), "until": "1s"}

	first, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "snooze_open_loop", Arguments: args})
	requirements.NoError(err)
	requirements.True(first.NeedsInput())
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "snooze_open_loop", Arguments: args, RequestState: first.RequestState,
		InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}},
	})
	requirements.NoError(err)
	assertions.True(result.IsError)
	requirements.Len(result.Content, 1)
	message, ok := result.Content[0].(*sdkmcp.TextContent)
	requirements.True(ok)
	assertions.Equal("snooze instant has passed; re-run with a later until", message.Text)
	assertions.Empty(b.calls)
}

func TestOpenLoopsCatalogReusesSchemaRoots(t *testing.T) {
	opts := loopMCPOptions(&loopMCPBackend{})
	first := operationCatalog(opts, &handlers{})
	second := operationCatalog(opts, &handlers{})
	roots := map[string]toolDefinition{}
	for _, definition := range first {
		roots[definition.name] = definition
	}
	require.Contains(t, roots, "list_open_loops")
	for _, definition := range second {
		if definition.name == "list_open_loops" || definition.name == "close_open_loop" {
			assert.Same(t, roots[definition.name].inputSchema, definition.inputSchema)
			assert.Same(t, roots[definition.name].outputSchema, definition.outputSchema)
		}
	}
}
