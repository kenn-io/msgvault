package identityindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChatMessagesAreClassifiedAsChat(t *testing.T) {
	for _, kind := range []string{"matrix", "tumblr"} {
		t.Run(kind, func(t *testing.T) {
			assert.True(t, IsChat(kind, "group_chat"))
			assert.True(t, IsChat(kind, "direct_chat"))
		})
	}
}
