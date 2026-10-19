package mcp

import (
	"encoding/json"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"testing"
	"time"
)

func TestInboxExistingTagToolConfirmsBeforeWrite(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	backend := &tagsToolBackend{}
	ct, st := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(ServeOptions{MessageTags: backend}, true).Connect(t.Context(), st, nil)
	requirements.NoError(err)
	defer func() { assertions.NoError(server.Close()) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "tag-confirm", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), ct, nil)
	requirements.NoError(err)
	defer func() { assertions.NoError(session.Close()) }()
	args := map[string]any{"message_id": float64(7), "add": []any{"Next"}}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	assertions.Nil(backend.change, "confirmation must precede provider access")
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	assertions.False(result.IsError)
	requirements.NotNil(backend.change)
	assertions.Equal([]string{"Next"}, backend.change.Add)
}

func TestInboxDelegatedTagsReuseExistingTool(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "native-2"}
	state := inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}
	backend := &inboxTestBackend{result: &inboxcontrol.Result{Before: &state, Projected: &state, PreviewToken: "signed"}}
	session := inboxSession(t, backend, true, inboxcontrol.OpTags)
	tools, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	requirements.Len(tools.Tools, 1)
	assertions.Equal(ToolUpdateMessageTags, tools.Tools[0].Name)
	data, err := json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(data, &targetArgs))
	args := map[string]any{"target": targetArgs, "add": []any{"Label_next"}}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	requirements.NoError(err)
	requirements.False(result.IsError)
	requirements.Len(backend.requests, 1)
	assertions.True(backend.requests[0].DryRun)
	assertions.Equal(inboxcontrol.OpTags, backend.requests[0].Operation)
	assertions.Equal([]string{"Label_next"}, backend.requests[0].Tags.Add)
	stateData, err := json.Marshal(state)
	requirements.NoError(err)
	var expected map[string]any
	requirements.NoError(json.Unmarshal(stateData, &expected))
	args["dry_run"], args["expected"], args["preview_token"], args["idempotency_key"] = false, expected, "signed", "tags-once"
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	assertions.Len(backend.requests, 1)
	confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	requirements.False(confirmed.IsError)
	requirements.Len(backend.requests, 2)
	assertions.False(backend.requests[1].DryRun)
	args["message_id"] = float64(2)
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolUpdateMessageTags, Arguments: args})
	requirements.NoError(err)
	assertions.True(invalid.IsError)
	assertions.Len(backend.requests, 2)
}
