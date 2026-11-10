package query

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestTumblrTextType(t *testing.T) {
	assert.True(t, IsTextMessageType("tumblr"))
	assert.Contains(t, KnownMessageTypes, "tumblr")
}
