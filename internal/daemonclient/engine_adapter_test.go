package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

func TestFastSearchScopedQueryStringIncludesMessageTypes(t *testing.T) {
	assert := assert.New(t)

	filterOnly, noMatches := fastSearchScopedQueryString(
		search.Parse("message_type:sms"),
		"message_type:sms",
		query.MessageFilter{},
	)
	assert.False(noMatches)
	assert.Equal("message_type:sms", filterOnly)

	withFilter, noMatches := fastSearchScopedQueryString(
		search.Parse("lunch"),
		"lunch",
		query.MessageFilter{MessageType: "sms"},
	)
	assert.False(noMatches)
	assert.Equal("lunch message_type:sms", withFilter)
}
