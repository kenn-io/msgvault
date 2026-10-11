package search

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAccountScopes(t *testing.T) {
	require := require.New(t)
	zero := int64(0)
	// Each rejected case is otherwise valid, so only its own rule can reject it.
	valid := func(n int) []AccountScope {
		scopes := make([]AccountScope, n)
		for i := range scopes {
			scopes[i] = AccountScope{Unattributed: true}
		}
		return scopes
	}
	addresses := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("a%d@example.org", i)
		}
		return out
	}
	for name, tc := range map[string]struct {
		scopes []AccountScope
		want   string
	}{
		"empty":              {[]AccountScope{{}}, "empty account scope"},
		"display name":       {[]AccountScope{{Addresses: []string{"Name <a@example.org>"}}}, "invalid account address"},
		"uppercase":          {[]AccountScope{{Addresses: []string{"A@example.org"}}}, "invalid account address"},
		"bad source":         {[]AccountScope{{SourceID: &zero, Unattributed: true}}, "invalid account scope source ID"},
		"too many":           {valid(17), "too many account scopes"},
		"too many addresses": {[]AccountScope{{Addresses: addresses(65)}}, "too many account addresses"},
	} {
		require.ErrorContains(ValidateAccountScopes(tc.scopes), tc.want, name)
	}
	require.NoError(ValidateAccountScopes(valid(16)), "sixteen scopes are allowed")
	require.NoError(ValidateAccountScopes([]AccountScope{{Addresses: addresses(64)}}), "64 addresses are allowed")
	require.NoError(ValidateAccountScopes([]AccountScope{{Unattributed: true}, {Addresses: []string{"a@example.org"}}}))
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
