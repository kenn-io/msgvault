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
	warnings         []string
	fail             bool
}

func (b *commentToolBackend) RunCommentAction(_ context.Context, id int64, r meta.ActionRequest) (*meta.ActionResult, error) {
	if r.DryRun {
		b.previews++
	} else {
		b.writes++
	}
	b.request = r
	result := &meta.ActionResult{MessageID: id, DryRun: r.DryRun, Confirm: "binding", Disclosure: meta.Disclosure{CommentID: "100_200_300", AuthorName: "Example Author", AssetName: "Example Page", Body: r.Body, Public: r.Action == meta.ActionPublicReply}, Warnings: append([]string{fmt.Sprintf("preview%d", b.previews)}, b.warnings...)}
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
	testMCPModerateConfirmedReinvoke(t, "delete")
}
func TestMCPModerateCommentBlockReinvokeMatches(t *testing.T) {
	testMCPModerateConfirmedReinvoke(t, "block")
}
func testMCPModerateConfirmedReinvoke(t *testing.T, action string) {
	t.Helper()
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
	args := map[string]any{"message_id": float64(7), "action": action}
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
	for _, name := range []string{ToolReplyToComment, ToolModerateComment, ToolUpdateCommentTriage, ToolListCommentQueue, ToolListCommentActions} {
		assert.NotContains(t, listed, name)
	}
	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	_, response := task3Serve(handler, task3ModernRequest("tools/call", ToolModerateComment, task3ToolCallBody(1, ToolModerateComment, `{"message_id":7,"action":"hide"}`)))
	require.NotNil(t, response.Error)
	_, response = task3Serve(handler, task3ModernRequest("tools/call", ToolReplyToComment, task3ToolCallBody(1, ToolReplyToComment, `{"message_id":7,"visibility":"public","body":"reply"}`)))
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
	assert.NotContains(t, read, ToolReplyToComment)
	assert.NotContains(t, read, ToolUpdateCommentTriage)
	write := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, write, ToolModerateComment)
	require.Contains(t, write, ToolReplyToComment)
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

func TestMCPReplyToCommentPublicDisclosure(t *testing.T) {
	testMCPReplyDisclosure(t, "public", "Publish a PUBLIC reply as Example Page to comment 100_200_300 by Example Author:\nExample reply")
}
func TestMCPReplyToCommentPrivateDisclosure(t *testing.T) {
	testMCPReplyDisclosure(t, "private", "Send ONE PRIVATE message to Example Author about comment 100_200_300 as Example Page:\nExample reply")
}
func testMCPReplyDisclosure(t *testing.T, visibility, want string) {
	t.Helper()
	b := &commentToolBackend{}
	opts := ServeOptions{CommentActions: b}
	args := map[string]any{"message_id": float64(7), "visibility": visibility, "body": "Example reply"}
	result := confirmedCallTool(t, opts, "reply_to_comment", args, false, func(prompt string) { assert.Equal(t, want, prompt) })
	assert.Equal(t, true, result["isError"])
	assert.Zero(t, b.writes)
	assert.Positive(t, b.previews)
	result = confirmedCallTool(t, opts, "reply_to_comment", args, true, func(prompt string) { assert.Equal(t, want, prompt) })
	assert.NotEqual(t, true, result["isError"])
	assert.Equal(t, 1, b.writes)
	assert.Equal(t, "binding", b.request.Confirm)
	assert.Equal(t, meta.ActionKind(visibility+"_reply"), b.request.Action)
	assert.Equal(t, "Example reply", b.request.Body)
	missing := rawCallTool(t, opts, "reply_to_comment", map[string]any{"message_id": float64(7), "body": "reply"})
	assert.Equal(t, true, missing["isError"])
	assert.Equal(t, 1, b.writes)
}
func TestMCPReplyToCommentReinvoke(t *testing.T) {
	b := &commentToolBackend{}
	ct, st := sdkmcp.NewInMemoryTransports()
	ss, err := newMCPServer(ServeOptions{CommentActions: b}, true).Connect(t.Context(), st, nil)
	require.NoError(t, err)
	defer func() { _ = ss.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "reply-confirmation-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	args := map[string]any{"message_id": float64(7), "visibility": "public", "body": "Example reply"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "reply_to_comment", Arguments: args})
	require.NoError(t, err)
	require.True(t, pending.NeedsInput())
	assert.Zero(t, b.writes)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "reply_to_comment", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, 1, b.writes)
	assert.Equal(t, 2, b.previews)
}

func TestMCPReplyToCommentAuthorWarningBeforeConfirmation(t *testing.T) {
	b := &commentToolBackend{warnings: []string{"prior_private_reply_to_author"}}
	result := confirmedCallTool(t, ServeOptions{CommentActions: b}, ToolReplyToComment, map[string]any{"message_id": float64(7), "visibility": "private", "body": "reply"}, false, func(prompt string) {
		assert.Contains(t, prompt, "Warning:")
		assert.Contains(t, prompt, "private reply to this author")
		assert.Contains(t, prompt, "past seven days")
	})
	assert.Equal(t, true, result["isError"])
	assert.Zero(t, b.writes)
}

func TestMCPModerateCommentBlockElicits(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(fmt.Sprint(accept), func(t *testing.T) {
			b := &commentToolBackend{}
			result := confirmedCallTool(t, ServeOptions{CommentActions: b}, ToolModerateComment, map[string]any{"message_id": float64(7), "action": "block"}, accept, func(prompt string) {
				assert.Equal(t, "Block Example Author from Example Page using comment 100_200_300. This affects the whole Page; unblock reverses it.", prompt)
			})
			if accept {
				assert.NotEqual(t, true, result["isError"])
				assert.Equal(t, 1, b.writes)
				assert.Equal(t, "binding", b.request.Confirm)
			} else {
				assert.Equal(t, true, result["isError"])
				assert.Zero(t, b.writes)
			}
			assert.Positive(t, b.previews)
		})
	}
}
