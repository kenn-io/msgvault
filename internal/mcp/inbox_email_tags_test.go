package mcp

import (
	"encoding/json"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"testing"
	"time"
)

func TestInboxExistingTagToolConfirmsBeforeWrite(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	backend := &tagsToolBackend{}
	ct, st := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(ServeOptions{MessageTags: backend}, true).Connect(t.Context(), st, nil)
	require.NoError(err)
	defer server.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "tag-confirm", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), ct, nil)
	require.NoError(err)
	defer session.Close()
	args := map[string]any{"message_id": float64(7), "add": []any{"Next"}}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	require.NoError(err)
	require.True(pending.NeedsInput())
	assert.Nil(backend.change, "confirmation must precede provider access")
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	require.NoError(err)
	assert.False(result.IsError)
	require.NotNil(backend.change)
	assert.Equal([]string{"Next"}, backend.change.Add)
}

func TestInboxDelegatedTagsReuseExistingTool(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "native-2"}
	state := inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}
	backend := &inboxTestBackend{result: &inboxcontrol.Result{Before: &state, Projected: &state, PreviewToken: "signed"}}
	session := inboxSession(t, backend, true, inboxcontrol.OpTags)
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(err)
	require.Len(tools.Tools, 1)
	assert.Equal(ToolUpdateMessageTags, tools.Tools[0].Name)
	data, err := json.Marshal(target)
	require.NoError(err)
	var targetArgs map[string]any
	require.NoError(json.Unmarshal(data, &targetArgs))
	args := map[string]any{"target": targetArgs, "add": []any{"Label_next"}}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	require.NoError(err)
	require.False(result.IsError)
	require.Len(backend.requests, 1)
	assert.True(backend.requests[0].DryRun)
	assert.Equal(inboxcontrol.OpTags, backend.requests[0].Operation)
	assert.Equal([]string{"Label_next"}, backend.requests[0].Tags.Add)
	stateData, err := json.Marshal(state)
	require.NoError(err)
	var expected map[string]any
	require.NoError(json.Unmarshal(stateData, &expected))
	args["dry_run"], args["expected"], args["preview_token"], args["idempotency_key"] = false, expected, "signed", "tags-once"
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	require.NoError(err)
	require.True(pending.NeedsInput())
	assert.Len(backend.requests, 1)
	confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	require.NoError(err)
	require.False(confirmed.IsError)
	require.Len(backend.requests, 2)
	assert.False(backend.requests[1].DryRun)
	args["message_id"] = float64(2)
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	require.NoError(err)
	assert.True(invalid.IsError)
	assert.Len(backend.requests, 2)
}
