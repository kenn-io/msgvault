package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/search"
)

func TestBuildSearchQueryStringIncludesMessageTypes(t *testing.T) {
	assert := assert.New(t)

	assert.Equal(
		"message_type:sms",
		buildSearchQueryString(search.Parse("message_type:sms")),
		"filter-only message type query",
	)
	assert.Equal(
		"lunch message_type:sms",
		buildSearchQueryString(search.Parse("message_type:sms lunch")),
		"message type with text term",
	)
}
