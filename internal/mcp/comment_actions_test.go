package mcp

import (
	"context"
	"fmt"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type commentToolBackend struct {
	previews, writes int
	request          meta.ActionRequest
	filter           meta.CommentQueueRequest
	expect           string
	fail             bool
}

func (b *commentToolBackend) RunCommentAction(_ context.Context, id int64, r meta.ActionRequest) (*meta.ActionResult, error) {
	if r.DryRun {
		b.previews++
	} else {
		b.writes++
	}
	b.request = r
	result := &meta.ActionResult{MessageID: id, DryRun: r.DryRun, Confirm: "binding", Disclosure: meta.Disclosure{CommentID: "100_200_300", AuthorName: "Example Author", AssetName: "Example Page"}, Warnings: []string{fmt.Sprintf("preview%d", b.previews)}}
	if b.fail {
		return result, &meta.ActionError{Code: "outcome_unknown", Message: "Inspect provider", Result: result}
	}
	return result, nil
}
func (b *commentToolBackend) ListCommentActions(context.Context, int64) ([]meta.Receipt, error) {
	return []meta.Receipt{{ID: 3, State: "succeeded"}}, nil
}
func (b *commentToolBackend) UpdateCommentTriage(_ context.Context, _ int64, state, expect string) (string, error) {
	b.writes++
	b.expect = expect
	return state, nil
}
func (b *commentToolBackend) ListCommentQueue(_ context.Context, f meta.CommentQueueRequest) (*meta.CommentQueueResponse, error) {
	b.filter = f
	return &meta.CommentQueueResponse{Items: []meta.CommentQueueItem{{MessageID: 7}}, NextBeforeID: 7}, nil
}
func TestMCPModerateCommentDeleteElicits(t *testing.T) {
	b := &commentToolBackend{}
	opts := ServeOptions{CommentActions: b}
	result := confirmedCallTool(t, opts, ToolModerateComment, map[string]any{"message_id": float64(7), "action": "delete"}, false, func(prompt string) {
		assert.Equal(t, "Delete comment 100_200_300 by Example Author on Example Page. History stays archived.", prompt)
	})
	assert.Equal(t, true, result["isError"])
	assert.Zero(t, b.writes)
	assert.Positive(t, b.previews)
}
func TestMCPModerateCommentDeleteReinvokeMatches(t *testing.T) {
	b := &commentToolBackend{}
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{CommentActions: b}, true).Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	defer func() { _ = serverSession.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "comment-confirmation-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	assert.Equal(t, modernProtocolVersion, serverSession.InitializeParams().ProtocolVersion)
	args := map[string]any{"message_id": float64(7), "action": "delete"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolModerateComment, Arguments: args})
	require.NoError(t, err)
	require.True(t, pending.NeedsInput())
	assert.Zero(t, b.writes)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolModerateComment, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, 1, b.writes)
	assert.Equal(t, 2, b.previews)
	assert.Equal(t, "binding", b.request.Confirm)
}
func TestMCPModerateCommentHideNoElicit(t *testing.T) {
	b := &commentToolBackend{}
	opts := ServeOptions{CommentActions: b}
	got := rawCallTool(t, opts, ToolModerateComment, map[string]any{"message_id": float64(7), "action": "hide"})
	assert.NotEqual(t, true, got["isError"])
	assert.Empty(t, got["inputRequests"])
	assert.Equal(t, 1, b.writes)
	assert.Zero(t, b.previews)
	b.fail = true
	got = rawCallTool(t, opts, ToolModerateComment, map[string]any{"message_id": float64(7), "action": "unhide"})
	assert.Equal(t, true, got["isError"])
	assert.Equal(t, "outcome_unknown", toolStructuredContent(t, got)["code"])
}
func TestMCPCommentToolsHiddenForDelegated(t *testing.T) {
	b := &commentToolBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, CommentActions: b, DelegatedOnly: true}
	listed := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{ToolModerateComment, ToolUpdateCommentTriage, ToolListCommentQueue, ToolListCommentActions} {
		assert.NotContains(t, listed, name)
	}
	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	_, response := task3Serve(handler, task3ModernRequest("tools/call", ToolModerateComment, task3ToolCallBody(1, ToolModerateComment, `{"message_id":7,"action":"hide"}`)))
	require.NotNil(t, response.Error)
	assert.Zero(t, b.writes)
	assert.Zero(t, b.previews)
}
func TestMCPCommentToolsAvailability(t *testing.T) {
	opts := ServeOptions{Engine: &querytest.MockEngine{}}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), ToolListCommentQueue)
	opts.CommentActions = &commentToolBackend{}
	read := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(t, read, ToolListCommentQueue)
	assert.Contains(t, read, ToolListCommentActions)
	assert.NotContains(t, read, ToolModerateComment)
	assert.NotContains(t, read, ToolUpdateCommentTriage)
	write := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, write, ToolModerateComment)
	assert.Equal(t, false, toolReadOnlyHint(t, write[ToolModerateComment]))
}
func TestMCPListCommentQueue(t *testing.T) {
	b := &commentToolBackend{}
	opts := ServeOptions{CommentActions: b}
	got := rawCallTool(t, opts, ToolListCommentQueue, map[string]any{"source": "app:9:page:100", "state": "handled", "limit": float64(1), "before_id": float64(3)})
	assert.NotEqual(t, true, got["isError"])
	assert.Equal(t, meta.CommentQueueRequest{Source: "app:9:page:100", State: "handled", Limit: 1, BeforeID: 3}, b.filter)
	assert.Equal(t, float64(7), toolStructuredContent(t, got)["next_before_id"])
	got = rawCallTool(t, opts, ToolUpdateCommentTriage, map[string]any{"message_id": float64(7), "state": "handled", "expect": "open"})
	assert.NotEqual(t, true, got["isError"])
	assert.Equal(t, "open", b.expect)
	got = rawCallTool(t, opts, ToolListCommentActions, map[string]any{"message_id": float64(7)})
	assert.NotEqual(t, true, got["isError"])
	assert.Contains(t, toolStructuredContent(t, got), "actions")
}
