package identityindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatrixMessagesAreClassifiedAsChat(t *testing.T) {
	assert.True(t, IsChat("matrix", "group_chat"))
	assert.True(t, IsChat("matrix", "direct_chat"))
}
