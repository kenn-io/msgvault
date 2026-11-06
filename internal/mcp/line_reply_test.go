package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPLineReplyConfirmationPromptAndCommand(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	args := map[string]any{"draft_id": "chat-draft-example", "revision": 1}
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"sent","accepted":true}`}}
	session := draftConfirmationSession(t, runner, "send-line-reply")
	accept := sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_line_reply", Arguments: args})
	require.NoError(err)
	prompt, ok := pending.InputRequests["confirm"].(*sdkmcp.ElicitParams)
	require.True(ok)
	assert.Contains(prompt.Message, "send_line_reply")
	assert.Contains(prompt.Message, "LINE")
	assert.Contains(prompt.Message, `"draft_id":"chat-draft-example"`)
	assert.Contains(prompt.Message, `"revision":1`)
	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_line_reply", Arguments: args, RequestState: pending.RequestState, InputResponses: accept})
	require.NoError(err)
	assert.Equal(DraftCommandRequest{Command: "send-line-reply", Positional: "chat-draft-example", Flags: map[string][]string{"revision": {"1"}}}, runner.request)
}

func TestMCPLineReplyOwnerCatalog(t *testing.T) {
	assert := assert.New(t)
	opts := ServeOptions{Drafts: &draftTestRunner{}, DraftCommands: []string{"send-line-reply"}}
	owner := operationCatalog(opts, nil)
	found := false
	for _, definition := range owner {
		if definition.name == "send_line_reply" {
			found = true
			require.NotNil(t, definition.annotations.DestructiveHint)
			assert.True(*definition.annotations.DestructiveHint)
		}
	}
	assert.True(found)
}
