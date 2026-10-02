package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountOperators(t *testing.T) {
	for _, text := range []string{"account:", "received:example.org", "received:fastmail-masked:owner@example.net", "account:@example.org", "account:a@example.org,b@example.org"} {
		t.Run(text, func(t *testing.T) {
			assert := assert.New(t)
			assert.Error(NewParser().Parse(text).Err())
		})
	}
	assert := assert.New(t)
	require := require.New(t)
	q := NewParser().Parse(`account:Work+tag@Example.org account:fastmail-masked:owner@example.net received:work+tag@example.org received:unattributed`)
	require.NoError(q.Err())
	require.Len(q.AccountScopes, 2)
	assert.Equal([]string{"work+tag@example.org"}, q.AccountScopes[0].Addresses)
	assert.Equal([]string{"fastmail-masked:owner@example.net"}, q.AccountScopes[0].Groups)
	assert.True(q.AccountScopes[1].Inbound)
	assert.True(q.AccountScopes[1].Unattributed)
	assert.False(q.IsEmpty())
	assert.Empty(q.TextTerms)
}

func TestAccountScopeTransportPreservesIntersection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	q := NewParser().Parse("account:work@example.org received:work@example.org")
	q.AccountScopes = append(q.AccountScopes, AccountScope{Addresses: []string{"other@example.org"}})
	got := NewParser().Parse(Format(q))
	require.NoError(got.Err())
	assert.Equal(q.AccountScopes, got.AccountScopes)
}

func TestAccountOperatorsIntersectWithTransportScopes(t *testing.T) {
	scoped := strings.Join(FormatAccountScopes([]AccountScope{{Addresses: []string{"work@example.org"}}, {Addresses: []string{"work@example.org"}}}), " ")
	for _, text := range []string{scoped + " account:other@example.org account:third@example.org", "account:other@example.org " + scoped + " account:third@example.org"} {
		t.Run(text, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			q := NewParser().Parse(text)
			require.NoError(q.Err())
			require.Len(q.AccountScopes, 3, "transport scopes must retain their intersections")
			var simple, transported int
			for _, scope := range q.AccountScopes {
				if slices.Equal(scope.Addresses, []string{"other@example.org", "third@example.org"}) {
					simple++
				}
				if slices.Equal(scope.Addresses, []string{"work@example.org"}) {
					transported++
				}
			}
			assert.Equal(1, simple, "repeated plain operators remain alternatives")
			assert.Equal(2, transported, "transported scopes remain separate")
		})
	}
}
