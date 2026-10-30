package mcp

import (
	"errors"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendReplyMCPConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, adopt := range []bool{false, true} {
		runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"draft_id":"chat-draft-synthetic","status":"sent_archive_pending","remote_status":"sent","local_status":"pending","media_id":"900719925474099312345","archive_complete":false}`}, err: &DraftCommandError{Message: "sent_archive_pending"}}
		args := map[string]any{"draft_id": "chat-draft-synthetic", "revision": 1}
		if adopt {
			args["adopt_media_id"] = "900719925474099312345"
		}
		session := draftConfirmationSession(t, runner, "send-reply")
		pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args})
		require.NoError(err)
		require.True(pending.NeedsInput())
		assert.Zero(runner.calls)
		declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "decline"}}})
		require.NoError(err)
		assert.True(declined.IsError)
		assert.Zero(runner.calls)
		accepted, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "send_reply", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
		require.NoError(err)
		assert.True(accepted.IsError)
		structured, ok := accepted.StructuredContent.(map[string]any)
		require.True(ok)
		assert.Equal("sent", structured["remote_status"])
		assert.Empty(runner.request.Positional)
		assert.Equal([]string{"chat-draft-synthetic"}, runner.request.Flags["draft"])
		assert.Equal([]string{"1"}, runner.request.Flags["revision"])
		if adopt {
			assert.Equal([]string{"900719925474099312345"}, runner.request.Flags["adopt-media-id"])
		}
	}
}

func TestSendReplyMCPDirectReply(t *testing.T) {
	assert := assert.New(t)
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"draft_id":"chat-draft-created","status":"complete","remote_status":"sent","local_status":"complete"}`}}
	args := map[string]any{"message_id": 101, "body": "Thanks!"}
	result := confirmedCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-reply"}}, "send_reply", args, true)
	assert.False(result["isError"].(bool))
	assert.Equal("101", runner.request.Positional)
	assert.Equal([]string{"Thanks!"}, runner.request.Flags["body"])
	assert.Empty(runner.request.Flags["source-id"])
}

func TestSendReplyMCPTruncatedResult(t *testing.T) {
	runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"remote_status":"sent"`}, err: errors.New("stream disconnected")}
	result := confirmedCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-reply"}}, "send_reply", map[string]any{"message_id": 101, "body": "Thanks!"}, true)
	assert.True(t, result["isError"].(bool))
	assert.Contains(t, strings.ToLower(result["content"].([]any)[0].(map[string]any)["text"].(string)), "unknown")
}

func TestSendReplyMCPReturnsReportedPreSendError(t *testing.T) {
	runner := &draftTestRunner{err: &DraftCommandError{Message: "replies are not supported for slack sources"}}
	result := confirmedCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-reply"}}, "send_reply", map[string]any{"message_id": 101, "body": "Thanks!"}, true)
	assert.True(t, result["isError"].(bool))
	assert.Contains(t, result["content"].([]any)[0].(map[string]any)["text"].(string), "replies are not supported for slack sources")
}

func TestSendReplyMCPOutcomesAndOwnerCatalog(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct {
		status, remote, local string
		failed                bool
	}{
		{"complete", "sent", "complete", false}, {"remote_unknown", "unknown", "pending", true}, {"container_saved", "unpublished", "pending", true}, {"send_failed", "unpublished", "draft_kept", true}, {"reply_not_permitted", "unpublished", "draft_kept", true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			runner := &draftTestRunner{result: DraftCommandResult{Stdout: `{"draft_id":"chat-draft-synthetic","status":"` + tc.status + `","remote_status":"` + tc.remote + `","local_status":"` + tc.local + `"}`}}
			if tc.failed {
				runner.err = &DraftCommandError{Message: tc.status}
			}
			result := confirmedCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-reply"}}, "send_reply", map[string]any{"draft_id": "chat-draft-synthetic", "revision": 1}, true)
			assert.Equal(tc.failed, result["isError"])
			assert.Equal(tc.remote, result["structuredContent"].(map[string]any)["remote_status"])
		})
	}
	runner := &draftTestRunner{}
	tools := rawListTools(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-reply"}, DelegatedOnly: true}, true)
	for _, tool := range tools {
		assert.NotEqual("send_reply", tool["name"])
	}
	assert.Zero(runner.calls)
	bypass := rawCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-reply"}}, "send_reply", map[string]any{"draft_id": "chat-draft-synthetic", "revision": 1, "confirmed": true})
	assert.True(bypass["isError"].(bool))
	assert.Zero(runner.calls)
}

func TestSendReplyMCPRejectsInvalidIntent(t *testing.T) {
	runner := &draftTestRunner{}
	for _, args := range []map[string]any{
		{"draft_id": "chat-draft-synthetic", "revision": 1, "body": "unreviewed"},
		{"message_id": 101, "body": "direct", "adopt_media_id": "500"},
		{"draft_id": "chat-draft-synthetic", "revision": 1, "adopt_media_id": ""},
		{"message_id": 101},
	} {
		result := rawCallTool(t, ServeOptions{Drafts: runner, DraftCommands: []string{"send-reply"}}, "send_reply", args)
		assert.True(t, result["isError"].(bool))
		assert.Empty(t, result["inputRequests"])
		assert.Zero(t, runner.calls)
	}
}
