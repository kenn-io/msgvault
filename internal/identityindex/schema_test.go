package identityindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatrixMessagesAreClassifiedAsChat(t *testing.T) {
	assert := assert.New(t)
	assert.True(IsChat("matrix", "group_chat"))
	assert.True(IsChat("matrix", "direct_chat"))
}

func TestRedditPublicHistoryStaysOutsideChat(t *testing.T) {
	assert := assert.New(t)
	assert.True(IsChat("reddit_chat", ""))
	assert.True(IsChat("reddit_pm", ""))
	assert.False(IsChat("reddit_post", "channel"))
	assert.False(IsChat("reddit_comment", "channel"))
}
