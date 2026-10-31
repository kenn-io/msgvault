package api

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSendReplyAllowlist(t *testing.T) {
	for _, args := range [][]string{
		{"send-reply", "--draft", "chat-draft-example", "--revision", "1", "--json"},
		{"send-reply", "42", "--body", "Thanks!"},
		{"send-reply", "--draft=chat-draft-example", "--revision=9223372036854775807", "--adopt-post=9001"},
	} {
		assert.True(t, cliRunCommandAllowed(args), "%v", args)
	}
	for _, args := range [][]string{
		{"send-reply"}, {"send-reply", "42"}, {"send-reply", "42", "--body", ""},
		{"send-reply", "--draft=chat-draft-example", "--revision=01"},
		{"send-reply", "42", "--body=Thanks", "--source-id=1"},
		{"send-reply", "42", "--body=Thanks", "--to=1001"},
		{"send-reply", "42", "--body=Thanks", "extra"},
		{"send-reply", "42", "--body=Thanks", "--adopt-post=9001"},
		{"send-reply", "--draft=chat-draft-example", "--revision=1", "--body=Thanks"},
		{"send-x-reply", "chat-draft-example", "--revision=1"},
	} {
		assert.False(t, cliRunCommandAllowed(args), "%v", args)
	}
}

func TestReplyPreparationOpenAPIContract(t *testing.T) {
	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/x/replies/prepare"]
	require.NotNil(t, path)
	require.NotNil(t, path.Post)
	assert.Equal(t, "prepareReply", path.Post.OperationID)
	request := doc.Components.Schemas.Map()["ReplyPrepareRequest"]
	require.NotNil(t, request)
	assert.Contains(t, request.Required, "draft_id")
	assert.Contains(t, request.Required, "revision")
	for _, key := range []string{"draft_id", "revision", "adopt_post"} {
		assert.Contains(t, request.Properties, key)
	}
	for _, key := range []string{"owner", "source_id", "text", "price", "allow_send"} {
		assert.NotContains(t, request.Properties, key)
	}
	result := doc.Components.Schemas.Map()["SendResult"]
	require.NotNil(t, result)
	for _, key := range []string{"status", "code", "remote_id", "attempt_id", "draft_id", "revision", "reserved_micros", "association"} {
		assert.Contains(t, result.Properties, key)
	}
	assert.False(t, delegatedOperationAllowed("prepareReply"))
	assert.False(t, remoteClientOperationAllowed("prepareReply", true))
}
