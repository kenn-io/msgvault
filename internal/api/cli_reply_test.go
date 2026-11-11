package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/x/replies/prepare"]
	_testRequire.NotNil(path)
	_testRequire.NotNil(path.Post)
	_testAssert.Equal("prepareReply", path.Post.OperationID)
	request := doc.Components.Schemas.Map()["ReplyPrepareRequest"]
	_testRequire.NotNil(request)
	_testAssert.Contains(request.Required, "draft_id")
	_testAssert.Contains(request.Required, "revision")
	for _, key := range []string{"draft_id", "revision", "adopt_post"} {
		_testAssert.Contains(request.Properties, key)
	}
	for _, key := range []string{"owner", "source_id", "text", "price", "allow_send"} {
		_testAssert.NotContains(request.Properties, key)
	}
	result := doc.Components.Schemas.Map()["SendResult"]
	_testRequire.NotNil(result)
	for _, key := range []string{"status", "code", "remote_id", "attempt_id", "draft_id", "revision", "reserved_micros", "association"} {
		_testAssert.Contains(result.Properties, key)
	}
	_testAssert.False(delegatedOperationAllowed("prepareReply"))
	_testAssert.False(remoteClientOperationAllowed("prepareReply", true))
}
