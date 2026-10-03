package search

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountScopeConditions(t *testing.T) {
	assert := assert.New(t)
	source := int64(7)
	conditions, args := AccountScopeConditions([]AccountScope{
		{SourceID: &source, Addresses: []string{"work@example.org"}, Unattributed: true},
		{Addresses: []string{"mask@example.org"}, Inbound: true},
	}, "m")
	assert.Equal([]string{
		"(m.source_id = ? AND (m.account_address IN (?) OR (m.account_address IS NULL AND COALESCE(m.message_type, '') IN ('', 'email', 'calendar_event'))))",
		"(((m.account_path = 'inbound' AND m.account_address IN (?))))",
	}, conditions)
	assert.Equal([]any{int64(7), "work@example.org", "mask@example.org"}, args)

	bound := AccountScopeConditionsBound([]AccountScope{{Addresses: []string{"a@example.org", "b@example.org"}}}, "m",
		func(v any) string { return "$" + fmt.Sprint(v) })
	assert.Equal([]string{"((m.account_address IN ($a@example.org,$b@example.org)))"}, bound)
}

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
