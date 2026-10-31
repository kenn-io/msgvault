package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPLineReplyRequiresExactConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	args := map[string]any{"draft_id": "chat-draft-example", "revision": 1}
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"sent","accepted":true}`}}
	unanswered := rawCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-line-reply"}}, "send_line_reply", args)
	require.NotEmpty(unanswered["inputRequests"])
	assert.Zero(runner.calls)
	session := draftConfirmationSession(t, runner, "send-line-reply")
	request := func(arguments map[string]any) *sdkmcp.CallToolResult {
		pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_line_reply", Arguments: arguments})
		require.NoError(err)
		require.True(pending.NeedsInput())
		return pending
	}
	accept := sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	pending := request(args)
	changed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_line_reply", Arguments: map[string]any{"draft_id": "chat-draft-example", "revision": 2}, RequestState: pending.RequestState, InputResponses: accept})
	require.NoError(err)
	assert.True(changed.IsError)
	assert.Zero(runner.calls)
	pending = request(args)
	declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_line_reply", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "decline"}}})
	require.NoError(err)
	assert.True(declined.IsError)
	assert.Zero(runner.calls)
	pending = request(args)
	prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	require.True(ok)
	assert.Contains(prompt.Message, "send_line_reply")
	assert.Contains(prompt.Message, "LINE")
	sent, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_line_reply", Arguments: args, RequestState: pending.RequestState, InputResponses: accept})
	require.NoError(err)
	assert.False(sent.IsError)
	assert.Equal(1, runner.calls)
	assert.Equal(DraftCommandRequest{Command: "send-line-reply", Positional: "chat-draft-example", Flags: map[string][]string{"revision": {"1"}}}, runner.request)
	replayed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_line_reply", Arguments: args, RequestState: pending.RequestState, InputResponses: accept})
	require.NoError(err)
	assert.True(replayed.IsError)
	assert.Equal(1, runner.calls)
}

func TestMCPLineReplyHiddenFromDelegatedCatalog(t *testing.T) {
	assert := assert.New(t)
	definitions := operationCatalog(ServeOptions{Drafts: &draftTestRunner{}, DraftCommands: []string{"send-line-reply", "draft-compose"}, DelegatedOnly: true}, nil)
	for _, definition := range definitions {
		assert.NotEqual("send_line_reply", definition.name)
	}
	owner := operationCatalog(ServeOptions{Drafts: &draftTestRunner{}, DraftCommands: []string{"send-line-reply"}}, nil)
	found := false
	for _, definition := range owner {
		if definition.name == "send_line_reply" {
			found = true
			require.NotNil(t, definition.annotations.DestructiveHint)
			assert.True(*definition.annotations.DestructiveHint)
		}
	}
	assert.True(found)
	opts := ServeOptions{Drafts: &draftTestRunner{}, DraftCommands: []string{"send-line-reply"}}
	assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), "send_line_reply")
	assert.Contains(toolsByName(t, rawListTools(t, opts, true)), "send_line_reply")
}
