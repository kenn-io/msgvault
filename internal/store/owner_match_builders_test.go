package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestOwnerMatchBuilders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fx := storetest.New(t)
	st, source := fx.Store, fx.Source.ID
	for _, address := range []string{"alice@example.com", "alias@example.com", "carol@example.com", "+15550100", "@alice:example.com"} {
		require.NoError(st.AddAccountIdentity(source, address, "manual"))
	}
	caseSender := fx.EnsureParticipant("Alice@Example.COM", "Alice", "example.com")
	aliasOnly, err := st.EnsureParticipantByPhone("+15550111", "Alias", "whatsapp")
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifier(aliasOnly, "email", "ALIAS@Example.com"))
	guarded := fx.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(st.SetParticipantIdentifier(guarded, "email", "carol@example.com"))
	phone, err := st.EnsureParticipantByPhone("+15550100", "Phone", "whatsapp")
	require.NoError(err)
	matrix := fx.EnsureParticipant("dave@example.com", "Dave", "example.com")
	require.NoError(st.SetParticipantIdentifier(matrix, "matrix", "@Alice:example.com"))

	ids := func(query string, args ...any) []int64 {
		rows, err := st.DB().Query(st.Rebind(query), args...)
		require.NoError(err)
		defer func() { _ = rows.Close() }()
		var got []int64
		for rows.Next() {
			var id int64
			require.NoError(rows.Scan(&id))
			got = append(got, id)
		}
		require.NoError(rows.Err())
		return got
	}
	identifierMatch := func(guard string) []int64 {
		return ids(`SELECT p.id FROM participants p WHERE EXISTS (
			SELECT 1 FROM participant_identifiers pi JOIN account_identities ai ON ai.source_id = ?
			WHERE pi.participant_id = p.id AND `+store.OwnerIdentifierMatch("pi.identifier_type", "pi.identifier_value", "ai.address", guard)+`)
			ORDER BY p.id`, source)
	}
	exists := func(identifierType, value string) bool {
		var found bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT EXISTS (SELECT 1 FROM account_identities ai WHERE `+
			store.OwnerIdentifierMatch("?", "?", "ai.address", "")+`)`), identifierType, value, identifierType, value).Scan(&found))
		return found
	}

	assert.Equal([]int64{caseSender}, ids(`SELECT p.id FROM participants p WHERE EXISTS (
		SELECT 1 FROM account_identities ai WHERE ai.source_id = ? AND `+store.OwnerEmailMatch("p.email_address", "ai.address")+`)
		ORDER BY p.id`, source))
	assert.Equal([]int64{aliasOnly, phone},
		identifierMatch(" AND (p.email_address IS NULL OR TRIM(p.email_address) = '')"),
		"email identifiers count only without a primary email; other types compare byte-exact")
	assert.Contains(identifierMatch(""), guarded, "without a guard an email identifier matches case-insensitively")
	assert.NotContains(identifierMatch(""), matrix, "non-email identifiers are case-sensitive")
	assert.Equal([]int64{caseSender, aliasOnly, phone}, ids(`SELECT sp.id FROM participants sp WHERE `+
		store.SenderOwnerFallback("sp.id", "?")+` ORDER BY sp.id`, source, source))
	assert.True(exists("email", "ALICE@EXAMPLE.COM"))
	assert.True(exists("phone", "+15550100"))
	assert.False(exists("matrix", "@Alice:example.com"))
	assert.False(exists("email", "nobody@example.com"))
}
