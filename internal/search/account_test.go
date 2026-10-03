package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountOperatorsParseExactAddresses(t *testing.T) {
	assert := assert.New(t)
	q := Parse("received:Work@Example.org received:mask@example.org account:inbox@example.net hello")
	require.NoError(t, q.Err())
	assert.Equal([]string{"work@example.org", "mask@example.org"}, q.ReceivedAddrs)
	assert.Equal([]string{"inbox@example.net"}, q.AccountAddrs)
	assert.Equal([]string{"hello"}, q.TextTerms)
	assert.True(q.HasOperators())
	assert.False(q.IsEmpty())

	only := Parse("account:inbox@example.net")
	assert.True(only.HasOperators())
	assert.False(only.IsEmpty())

	assertQueryEqual(t, *Parse(Format(q)), *q)
}

func TestAccountOperatorsRejectInexactValues(t *testing.T) {
	for _, query := range []string{
		"received:work",
		"account:@example.org",
		`account:"Name <a@example.org>"`,
		"received:",
	} {
		t.Run(query, func(t *testing.T) {
			assert := assert.New(t)
			q := Parse(query)
			require.Error(t, q.Err())
			assert.Contains(q.Err().Error(), "expected an exact email address")
			assert.Empty(q.AccountAddrs)
			assert.Empty(q.ReceivedAddrs)
		})
	}
}

func TestAccountConditions(t *testing.T) {
	assert := assert.New(t)
	conditions, args := AccountConditions(Parse("received:a@example.org received:b@example.org account:c@example.org"), "m")
	assert.Equal([]string{
		"m.account_address IN (?)",
		"(m.account_path = 'inbound' AND m.account_address IN (?,?))",
	}, conditions)
	assert.Equal([]any{"c@example.org", "a@example.org", "b@example.org"}, args)

	conditions, args = AccountConditions(Parse("hello"), "msg")
	assert.Empty(conditions)
	assert.Empty(args)
}
