package identityindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatrixMessagesAreClassifiedAsChat(t *testing.T) {
	assert.True(t, IsChat("matrix", "group_chat"))
	assert.True(t, IsChat("matrix", "direct_chat"))
}

func TestRedditPublicHistoryStaysOutsideChat(t *testing.T) {
	assert := assert.New(t)
	assert.True(IsChat("reddit_chat", "chat"))
	assert.True(IsChat("reddit_pm", "chat"))
	assert.False(IsChat("reddit_post", ""))
	assert.False(IsChat("reddit_comment", ""))
}
