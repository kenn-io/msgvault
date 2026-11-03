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
	disclosure       *meta.Disclosure
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
	if b.disclosure != nil {
		result.Disclosure = *b.disclosure
		result.Disclosure.Body = r.Body
	}
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
	assertions := assert.New(t)
	requirements := require.New(t)
	b := &commentToolBackend{}
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{CommentActions: b}, true).Connect(t.Context(), serverTransport, nil)
	requirements.NoError(err)
	defer func() { _ = serverSession.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "comment-confirmation-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	requirements.NoError(err)
	defer func() { _ = session.Close() }()
	assertions.Equal(modernProtocolVersion, serverSession.InitializeParams().ProtocolVersion)
	args := map[string]any{"message_id": float64(7), "action": "delete"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolModerateComment, Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	assertions.Zero(b.writes)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolModerateComment, Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	assertions.False(result.IsError)
	assertions.Equal(1, b.writes)
	assertions.Equal(2, b.previews)
	assertions.Equal("binding", b.request.Confirm)
}
func TestMCPModerateCommentHideNoElicit(t *testing.T) {
	assert := assert.New(t)
	b := &commentToolBackend{}
	opts := ServeOptions{CommentActions: b}
	got := rawCallTool(t, opts, ToolModerateComment, map[string]any{"message_id": float64(7), "action": "hide"})
	assert.NotEqual(true, got["isError"])
	assert.Empty(got["inputRequests"])
	assert.Equal(1, b.writes)
	assert.Zero(b.previews)
	b.fail = true
	got = rawCallTool(t, opts, ToolModerateComment, map[string]any{"message_id": float64(7), "action": "unhide"})
	assert.Equal(true, got["isError"])
	assert.Equal("outcome_unknown", toolStructuredContent(t, got)["code"])
}
func TestMCPCommentToolsHiddenForDelegated(t *testing.T) {
	assert := assert.New(t)
	b := &commentToolBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, CommentActions: b, DelegatedOnly: true}
	listed := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{ToolReplyToComment, ToolModerateComment, ToolUpdateCommentTriage, ToolListCommentQueue, ToolListCommentActions} {
		assert.NotContains(listed, name)
	}
	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	_, response := task3Serve(handler, task3ModernRequest("tools/call", ToolModerateComment, task3ToolCallBody(1, ToolModerateComment, `{"message_id":7,"action":"hide"}`)))
	require.NotNil(t, response.Error)
	_, response = task3Serve(handler, task3ModernRequest("tools/call", ToolReplyToComment, task3ToolCallBody(1, ToolReplyToComment, `{"message_id":7,"visibility":"public","body":"reply"}`)))
	require.NotNil(t, response.Error)
	assert.Zero(b.writes)
	assert.Zero(b.previews)
}
func TestMCPCommentToolsAvailability(t *testing.T) {
	assert := assert.New(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}}
	assert.NotContains(toolsByName(t, rawListTools(t, opts, true)), ToolListCommentQueue)
	opts.CommentActions = &commentToolBackend{}
	read := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(read, ToolListCommentQueue)
	assert.Contains(read, ToolListCommentActions)
	assert.NotContains(read, ToolModerateComment)
	assert.NotContains(read, ToolReplyToComment)
	assert.NotContains(read, ToolUpdateCommentTriage)
	write := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, write, ToolModerateComment)
	require.Contains(t, write, ToolReplyToComment)
	assert.Equal(false, toolReadOnlyHint(t, write[ToolModerateComment]))
}
func TestMCPListCommentQueue(t *testing.T) {
	assert := assert.New(t)
	b := &commentToolBackend{}
	opts := ServeOptions{CommentActions: b}
	got := rawCallTool(t, opts, ToolListCommentQueue, map[string]any{"source": "app:9:page:100", "state": "handled", "limit": float64(1), "before_id": float64(3)})
	assert.NotEqual(true, got["isError"])
	assert.Equal(meta.CommentQueueRequest{Source: "app:9:page:100", State: "handled", Limit: 1, BeforeID: 3}, b.filter)
	assert.InDelta(float64(7), toolStructuredContent(t, got)["next_before_id"], 0)
	got = rawCallTool(t, opts, ToolUpdateCommentTriage, map[string]any{"message_id": float64(7), "state": "handled", "expect": "open"})
	assert.NotEqual(true, got["isError"])
	assert.Equal("open", b.expect)
	got = rawCallTool(t, opts, ToolListCommentActions, map[string]any{"message_id": float64(7)})
	assert.NotEqual(true, got["isError"])
	assert.Contains(toolStructuredContent(t, got), "actions")
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
	assertions := assert.New(t)
	requirements := require.New(t)
	b := &commentToolBackend{}
	ct, st := sdkmcp.NewInMemoryTransports()
	ss, err := newMCPServer(ServeOptions{CommentActions: b}, true).Connect(t.Context(), st, nil)
	requirements.NoError(err)
	defer func() { _ = ss.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "reply-confirmation-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), ct, nil)
	requirements.NoError(err)
	defer func() { _ = session.Close() }()
	args := map[string]any{"message_id": float64(7), "visibility": "public", "body": "Example reply"}
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "reply_to_comment", Arguments: args})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	assertions.Zero(b.writes)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "reply_to_comment", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	assertions.False(result.IsError)
	assertions.Equal(1, b.writes)
	assertions.Equal(2, b.previews)
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

func TestMCPReplyToInstagramMentionDisclosure(t *testing.T) {
	for _, tc := range []struct{ name, marker, id, want string }{
		{"caption", "instagram_caption_mention", "300", "Publish a PUBLIC reply as Example Instagram to Instagram caption mention on media 300 (author unavailable):\nExact example reply"},
		{"comment", "instagram_comment_mention", "400", "Publish a PUBLIC reply as Example Instagram to Instagram comment mention 400 (author unavailable):\nExact example reply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			b := &commentToolBackend{warnings: []string{tc.marker}, disclosure: &meta.Disclosure{Provider: "instagram", AssetName: "Example Instagram", CommentID: tc.id, Public: true}}
			args := map[string]any{"message_id": float64(7), "visibility": "public", "body": "Exact example reply"}
			declined := confirmedCallTool(t, ServeOptions{CommentActions: b}, ToolReplyToComment, args, false, func(prompt string) { assertions.Equal(tc.want, prompt) })
			assertions.Equal(true, declined["isError"])
			assertions.Zero(b.writes)
			accepted := confirmedCallTool(t, ServeOptions{CommentActions: b}, ToolReplyToComment, args, true, func(prompt string) { assertions.Equal(tc.want, prompt) })
			assertions.NotEqual(true, accepted["isError"])
			assertions.Equal(1, b.writes)
			assertions.Equal(meta.ActionRequest{MessageID: 7, Action: meta.ActionPublicReply, Body: "Exact example reply", Confirm: "binding"}, b.request)
		})
	}
}
