package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAccountScopes(t *testing.T) {
	zero := int64(0)
	for name, scopes := range map[string][]AccountScope{
		"empty":        {{}},
		"display name": {{Addresses: []string{"Name <a@example.org>"}}},
		"uppercase":    {{Addresses: []string{"A@example.org"}}},
		"bad source":   {{SourceID: &zero, Unattributed: true}},
		"too many":     make([]AccountScope, 17),
	} {
		require.Error(t, ValidateAccountScopes(scopes), name)
	}
	require.NoError(t, ValidateAccountScopes([]AccountScope{{Unattributed: true}, {Addresses: []string{"a@example.org"}}}))
}

func TestAccountScopesFromQuery(t *testing.T) {
	q := Parse("account:a@example.org received:b@example.org received:c@example.org")
	require.NoError(t, q.Err())
	assert.Equal(t, []AccountScope{
		{Addresses: []string{"a@example.org"}},
		{Addresses: []string{"b@example.org", "c@example.org"}, Inbound: true},
	}, AccountScopesFromQuery(q))
	assert.Empty(t, Format(&Query{AccountScopes: []AccountScope{{Unattributed: true}}}),
		"scopes never appear in query text")
}
