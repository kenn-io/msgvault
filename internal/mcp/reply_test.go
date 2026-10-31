package mcp

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Discovery uses the real catalog/SDK and never calls this backend, matching
// the memory fixture's stand-ins. Actual send behavior is exercised through
// the real daemon, Sender, HTTP and Store in the command package.
func TestReplyCatalogRequiresOwnerCapabilityAndWriteTransport(t *testing.T) {
	capable := ServeOptions{Replies: struct{ ReplySender }{}}
	tools := toolsByName(t, rawListTools(t, capable, true))
	require.Contains(t, tools, ToolSendReply)
	assert.NotContains(t, tools, "send_x_reply")
	assert.Contains(t, tools[ToolSendReply]["description"], "x_reply_send")
	for _, field := range []string{"message_id", "body", "draft_id", "revision", "adopt_post"} {
		assert.NotNil(t, toolInputProperty(t, tools[ToolSendReply], field))
	}
	schema, ok := tools[ToolSendReply]["inputSchema"].(map[string]any)
	require.True(t, ok)
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	for _, field := range []string{"source_id", "to", "owner", "retry", "accept_url_cost"} {
		assert.NotContains(t, properties, field)
	}
	assert.NotContains(t, toolsByName(t, rawListTools(t, capable, false)), ToolSendReply)
	capable.DelegatedOnly = true
	assert.NotContains(t, toolsByName(t, rawListTools(t, capable, true)), ToolSendReply)
	assert.NotContains(t, toolsByName(t, rawListTools(t, ServeOptions{}, true)), ToolSendReply)
}
func TestReplyArgumentsUseExactIDsAndTransportValidator(t *testing.T) {
	for _, number := range []any{float64(1), float64(9007199254740991), "9223372036854775807"} {
		parsed, err := parseReplyArguments(map[string]any{"draft_id": "chat-draft-example", "revision": number})
		require.NoError(t, err)
		assert.Positive(t, parsed.Revision)
	}
	direct, err := parseReplyArguments(map[string]any{"message_id": "9223372036854775807", "body": strings.Repeat("🙂", 4096)})
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), direct.MessageID)
	assert.Len(t, direct.Body, 16*1024)
	for _, arguments := range []map[string]any{
		{}, {"message_id": float64(1)}, {"draft_id": "chat-draft-example"},
		{"message_id": float64(1), "body": "safe", "source_id": float64(1)},
		{"message_id": float64(1), "body": "safe", "to": "1001"},
		{"message_id": float64(1), "body": "safe", "adopt_post": "9001"},
		{"message_id": float64(1), "body": "safe", "revision": float64(1)},
		{"draft_id": "chat-draft-example", "revision": float64(1), "body": "safe"},
		{"draft_id": "chat-draft-example", "revision": float64(9007199254740992)},
		{"draft_id": "chat-draft-example", "revision": "01"},
		{"draft_id": "chat-draft-example", "revision": "9223372036854775808"},
		{"draft_id": "chat-draft-example", "revision": float64(1), "adopt_post": "01"},
		{"message_id": float64(1), "body": "\x00"}, {"message_id": float64(1), "body": " \n\t"}, {"message_id": float64(1), "body": strings.Repeat("x", 16*1024+1)},
	} {
		_, err := parseReplyArguments(arguments)
		require.Error(t, err)
	}
}
