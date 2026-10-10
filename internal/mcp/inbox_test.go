package mcp

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxTestBackend struct {
	requests []inboxcontrol.Request
	result   *inboxcontrol.Result
	err      error
}

func (b *inboxTestBackend) ControlInbox(_ context.Context, r inboxcontrol.Request) (*inboxcontrol.Result, error) {
	b.requests = append(b.requests, r)
	return b.result, b.err
}
func inboxSession(t *testing.T, b *inboxTestBackend, writes bool, ops ...inboxcontrol.Operation) *sdkmcp.ClientSession {
	t.Helper()
	ct, st := sdkmcp.NewInMemoryTransports()
	ss, err := newMCPServer(ServeOptions{CalendarOnly: true, Inbox: b, InboxOperations: ops}, writes).Connect(t.Context(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	c := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "inbox-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	cs, err := c.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
func TestInboxCatalogFiltersCallerAndWrites(t *testing.T) {
	for _, writes := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[writes], func(t *testing.T) {
			s := inboxSession(t, &inboxTestBackend{}, writes, inboxcontrol.OpGetState, inboxcontrol.OpArchive)
			tools, err := s.ListTools(t.Context(), nil)
			require.NoError(t, err)
			var names []string
			for _, tool := range tools.Tools {
				names = append(names, tool.Name)
			}
			want := []string{"inbox_get_state"}
			if writes {
				want = []string{"inbox_archive", "inbox_get_state"}
			}
			assert.Equal(t, want, names)
		})
	}
}
func TestInboxPreviewConfirmationAndUnknownReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "m2"}
	state := inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}
	b := &inboxTestBackend{result: &inboxcontrol.Result{Before: &state, Projected: &state, PreviewToken: "signed"}}
	s := inboxSession(t, b, true, inboxcontrol.OpArchive)
	raw, err := json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(raw, &targetArgs))
	args := map[string]any{"target": targetArgs}
	preview, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: args})
	requirements.NoError(err)
	requirements.False(preview.IsError)
	requirements.Len(b.requests, 1)
	assertions.True(b.requests[0].DryRun)
	raw, err = json.Marshal(state)
	requirements.NoError(err)
	var expected map[string]any
	requirements.NoError(json.Unmarshal(raw, &expected))
	args["dry_run"] = false
	args["expected"] = expected
	args["preview_token"] = "signed"
	args["idempotency_key"] = "once"
	pending, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	assertions.Len(b.requests, 1)
	b.result = &inboxcontrol.Result{Receipt: &inboxcontrol.Receipt{ID: "r1", Status: inboxcontrol.StatusUnknown}}
	b.err = inboxcontrol.ErrOutcomeUnknown
	params := &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}}
	result, err := s.CallTool(t.Context(), params)
	requirements.NoError(err)
	assertions.True(result.IsError)
	requirements.Len(b.requests, 2, "%+v", result)
	assertions.False(b.requests[1].DryRun)
	encoded, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(encoded), "r1")
	assertions.Contains(string(encoded), "unknown")
	replay, err := s.CallTool(t.Context(), params)
	requirements.NoError(err)
	assertions.True(replay.IsError)
	assertions.Len(b.requests, 2)
}

func TestInboxRejectsInvalidArgumentsBeforeBackend(t *testing.T) {
	target := map[string]any{"source_id": 1, "source_type": "gmail", "source_identifier": "reader@example.com", "account_id": "reader@example.com", "scope": "message", "item_id": 2, "provider_id": "m2"}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"override operation", map[string]any{"target": target, "operation": "set-read"}},
		{"null preview", map[string]any{"target": target, "dry_run": nil}},
		{"unsafe ID", map[string]any{"target": map[string]any{"source_id": 9007199254740992, "source_type": "gmail", "source_identifier": "reader@example.com", "account_id": "reader@example.com", "scope": "message", "item_id": 2, "provider_id": "m2"}}},
		{"unsigned apply", map[string]any{"target": target, "dry_run": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &inboxTestBackend{result: &inboxcontrol.Result{}}
			s := inboxSession(t, b, true, inboxcontrol.OpArchive)
			r, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: tc.args})
			require.NoError(t, err)
			assert.True(t, r.IsError)
			assert.Empty(t, b.requests)
		})
	}
}
func TestInboxCatalogPreservesCalendarAndDraftUnion(t *testing.T) {
	assertions := assert.New(t)

	opts := ServeOptions{CalendarOnly: true, Calendar: &calendarMCPFake{}, Drafts: &draftTestRunner{}, DraftCommands: []string{"draft-compose"}, Inbox: &inboxTestBackend{}, InboxOperations: []inboxcontrol.Operation{inboxcontrol.OpGetState, inboxcontrol.OpGetState}}
	var names []string
	for _, d := range operationCatalog(opts, nil) {
		names = append(names, d.name)
	}
	assertions.Contains(names, "draft_compose")
	assertions.Contains(names, "inbox_get_state")
	assertions.NotContains(names, ToolSearchMessages)
	count := 0
	for _, name := range names {
		if name == "inbox_get_state" {
			count++
		}
	}
	assertions.Equal(1, count)
	assertions.Greater(len(names), 2, "calendar tools must coexist with drafts and inbox")
}

func TestInboxConfirmationRejectsChangedIntent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "m2"}
	argsBytes, err := json.Marshal(map[string]any{"target": target, "dry_run": false, "expected": inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}, "preview_token": "signed", "idempotency_key": "first"})
	requirements.NoError(err)
	var args map[string]any
	requirements.NoError(json.Unmarshal(argsBytes, &args))
	b := &inboxTestBackend{}
	s := inboxSession(t, b, true, inboxcontrol.OpArchive)
	pending, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	args["idempotency_key"] = "changed"
	result, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	assertions.True(result.IsError)
	assertions.Empty(b.requests)
}
func TestInboxAllOperationSchemasRegister(t *testing.T) {
	var ops []inboxcontrol.Operation
	for op := range stableInboxDefinitions {
		ops = append(ops, op)
	}
	s := inboxSession(t, &inboxTestBackend{}, true, ops...)
	tools, err := s.ListTools(t.Context(), nil)
	require.NoError(t, err)
	assert.Len(t, tools.Tools, 12)
}

func TestInboxOutcomeCodesPreserveReceipt(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{inboxcontrol.ErrNoWrite, "rejected-no-write"},
		{inboxcontrol.ErrReconcileOnly, "reconcile-only"},
		{inboxcontrol.ErrInternal, "internal"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			b := &inboxTestBackend{result: &inboxcontrol.Result{Receipt: &inboxcontrol.Receipt{ID: "r1", Status: inboxcontrol.StatusReconcileOnly}}, err: tc.err}
			s := inboxSession(t, b, true, inboxcontrol.OpReconcile)
			result, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_reconcile", Arguments: map[string]any{"receipt_id": "r1"}})
			requirements.NoError(err)
			requirements.True(result.IsError)
			raw, err := json.Marshal(result.StructuredContent)
			requirements.NoError(err)
			var response inboxToolResponse
			requirements.NoError(json.Unmarshal(raw, &response))
			assertions.Equal(tc.code, response.Error)
			requirements.NotNil(response.Result)
			requirements.NotNil(response.Result.Receipt)
			assertions.Equal("r1", response.Result.Receipt.ID)
			assertions.Len(b.requests, 1)
		})
	}
}
