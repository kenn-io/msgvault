package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type draftTestRunner struct {
	result  DraftCommandResult
	err     error
	request DraftCommandRequest
}

func (r *draftTestRunner) RunDraftCommand(_ context.Context, request DraftCommandRequest) (DraftCommandResult, error) {
	r.request = request
	return r.result, r.err
}

func TestDraftToolCatalogFollowsCommandsAndWriteClass(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commands := []string{"draft-reply", "draft-compose", "draft-forward", "draft-get", "draft-edit", "draft-delete", "draft-recover", "draft-send-as", "unknown"}
	opts := ServeOptions{Drafts: &draftTestRunner{}, DraftCommands: commands}
	names := func(tools []map[string]any) []string {
		var names []string
		for _, tool := range tools {
			name, ok := tool["name"].(string)
			require.True(ok)
			if strings.HasPrefix(name, "draft_") {
				names = append(names, name)
			}
		}
		return names
	}
	owner := rawListTools(t, opts, true)
	assert.Equal([]string{"draft_compose", "draft_delete", "draft_edit", "draft_forward", "draft_get", "draft_recover", "draft_reply", "draft_send_as"}, names(owner))
	assert.Equal([]string{"draft_get", "draft_send_as"}, names(rawListTools(t, opts, false)))
	for _, name := range []string{ToolDraftDelete, ToolDraftRecover} {
		annotations, ok := toolsByName(t, owner)[name]["annotations"].(map[string]any)
		require.True(ok)
		assert.Equal(true, annotations["destructiveHint"])
	}
	opts.DelegatedOnly = true
	opts.DraftCommands = []string{"draft-reply", "draft-compose", "draft-get", "draft-edit", "draft-delete", "draft-recover"}
	tools := rawListTools(t, opts, true)
	assert.Len(tools, 6)
	assert.NotContains(toolsByName(t, tools), ToolGetStats)
	response := rawModernCall(t, opts, HTTPOptions{AllowWrites: true}, "resources/templates/list", nil)
	assert.Empty(response.Result["resourceTemplates"])
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Drafts: &draftTestRunner{}, DraftCommands: commands}, true).Connect(t.Context(), serverTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "draft-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(err)
	var stdioDrafts []string
	for _, tool := range listed.Tools {
		if strings.HasPrefix(tool.Name, "draft_") {
			stdioDrafts = append(stdioDrafts, tool.Name)
		}
	}
	assert.Len(stdioDrafts, 8)
}

func TestDraftToolResultShapes(t *testing.T) {
	for _, tc := range []struct {
		name, stdout string
		err          error
		want         any
		text         string
	}{
		{name: "object", stdout: `{"status":"created"}`, want: map[string]any{"status": "created"}},
		{name: "array", stdout: `[{"draft_id":"d1"}]`, want: map[string]any{"data": []any{map[string]any{"draft_id": "d1"}}}},
		{name: "daemon text", err: &DraftCommandError{Message: "not_permitted", Stderr: "{\"status\":\"pending\"}\n"}, text: "not_permitted\n{\"status\":\"pending\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			runner := &draftTestRunner{result: DraftCommandResult{Stdout: tc.stdout}, err: tc.err}
			result := rawCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"draft-reply"}}, ToolDraftReply, map[string]any{"message_id": 7, "source_id": 9, "body": "reply", "all": false})
			assert.Equal("7", runner.request.Positional)
			assert.Equal([]string{"9"}, runner.request.Flags["source-id"])
			assert.NotContains(runner.request.Flags, "all")
			if tc.text != "" {
				assert.Equal(true, result["isError"])
				content, ok := result["content"].([]any)
				require.True(ok)
				require.NotEmpty(content)
				text, ok := content[0].(map[string]any)
				require.True(ok)
				assert.Equal(tc.text, text["text"])
			} else {
				assert.Equal(tc.want, result["structuredContent"])
			}
		})
	}
	for _, name := range []string{"dial fault private-host", "truncated response private-body", "malformed HTML private-body", "context canceled"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			serverSession, err := newMCPServer(ServeOptions{Drafts: &draftTestRunner{err: errors.New(name)}, DraftCommands: []string{"draft-get"}, DelegatedOnly: true}, true).Connect(t.Context(), serverTransport, nil)
			require.NoError(err)
			t.Cleanup(func() { _ = serverSession.Close() })
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "draft-error-test", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), clientTransport, nil)
			require.NoError(err)
			t.Cleanup(func() { _ = session.Close() })
			_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolDraftGet, Arguments: map[string]any{"draft_id": "d1"}})
			require.ErrorContains(err, "internal server error")
			assert.NotContains(err.Error(), name)
		})
	}
}
