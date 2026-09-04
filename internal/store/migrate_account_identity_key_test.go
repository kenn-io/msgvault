package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestAccountIdentityAddressKeyBackfillMergesCaseVariantEmails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "backfill-case@example.com")
	require.NoError(err, "GetOrCreateSource")

	// Two case variants of one logical email identity, written as a
	// previous-release binary would: address_key omitted, so it lands as ''.
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO account_identities (source_id, address, source_signal, confirmed_at)
		 VALUES (?, ?, ?, ?)`),
		src.ID, "Alice@Example.com", "manual", "2024-01-02 03:04:05+00:00")
	require.NoError(err, "seed first case variant")
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO account_identities (source_id, address, source_signal, confirmed_at)
		 VALUES (?, ?, ?, ?)`),
		src.ID, "alice@example.com", "header", "2024-05-06 07:08:09+00:00")
	require.NoError(err, "seed second case variant")

	require.NoError(st.InitSchema(), "reinit schema to run the key repair")

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1, "case variants must collapse to one row")
	got := identities[0]
	assert.Equal("Alice@Example.com", got.Address,
		"the earliest-confirmed row keeps the logical identity and its display casing")
	assert.Equal("header,manual", got.SourceSignal, "signal sets must union")

	var key string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT address_key FROM account_identities WHERE source_id = ? AND address = ?`),
		src.ID, "Alice@Example.com").Scan(&key))
	assert.Equal("alice@example.com", key, "backfilled key must be the comparison-canonical form")
}

func TestAccountIdentityAddressKeyBackfillPreservesNonEmailCase(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("matrix", "backfill-mxid@example.com")
	require.NoError(err, "GetOrCreateSource")

	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO account_identities (source_id, address, source_signal)
		 VALUES (?, ?, ?)`),
		src.ID, "@User:server.org", "manual")
	require.NoError(err, "seed mixed-case MXID")
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO account_identities (source_id, address, source_signal)
		 VALUES (?, ?, ?)`),
		src.ID, "@user:server.org", "manual")
	require.NoError(err, "seed lowercase MXID")

	require.NoError(st.InitSchema(), "reinit schema to run the key repair")

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 2, "non-email identifiers are case-sensitive and must not merge")
	for _, ai := range identities {
		var key string
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT address_key FROM account_identities WHERE source_id = ? AND address = ?`),
			src.ID, ai.Address).Scan(&key))
		assert.Equal(ai.Address, key, "non-email key must preserve case verbatim")
	}
}

func TestAccountIdentityKeyIndexRejectsKeyedCaseVariantDuplicate(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "index-reject@example.com")
	require.NoError(err, "GetOrCreateSource")

	require.NoError(st.AddAccountIdentity(src.ID, "bob@example.com", "manual"))

	// A direct SQL writer supplying the derived key for a case variant must
	// hit the partial unique index: this is the schema-level enforcement the
	// application-level compare could not provide.
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO account_identities (source_id, address, address_key, source_signal)
		 VALUES (?, ?, ?, ?)`),
		src.ID, "Bob@Example.com", "bob@example.com", "raw")
	require.Error(err, "keyed case-variant duplicate must violate the unique index")
}

func TestAccountIdentityLegacyOmittedKeyInsertSucceedsAndIsRepaired(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "legacy-writer@example.com")
	require.NoError(err, "GetOrCreateSource")

	require.NoError(st.AddAccountIdentity(src.ID, "Carol@Example.com", "manual"))

	// A previous-release binary inserts with its old column list. The ''
	// default keeps the write working (the partial index exempts ''), even
	// though a keyed row for the same logical identity already exists.
	_, err = st.DB().Exec(st.Rebind(
		`INSERT INTO account_identities (source_id, address, source_signal)
		 VALUES (?, ?, ?)`),
		src.ID, "carol@example.com", "legacy")
	require.NoError(err, "previous-release column list must keep working")

	require.NoError(st.InitSchema(), "reinit schema to run the key repair")

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1, "legacy duplicate must merge into the keyed row")
	assert.Equal("Carol@Example.com", identities[0].Address,
		"the row already carrying the correct key survives")
	assert.Equal("legacy,manual", identities[0].SourceSignal, "signal sets must union")
}

func TestAddAccountIdentityCaseVariantsMergeToOneRow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "add-case-variant@example.com")
	require.NoError(err, "GetOrCreateSource")

	require.NoError(st.AddAccountIdentity(src.ID, "Dana@Example.com", "manual"))
	require.NoError(st.AddAccountIdentity(src.ID, "dana@example.com", "header"))

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1, "case-variant confirmations must merge, not diverge")
	assert.Equal("Dana@Example.com", identities[0].Address,
		"display casing stays as first written")
	assert.Equal("header,manual", identities[0].SourceSignal)

	// Case-aware removal still works after the keyed lookup change.
	removed, err := st.RemoveAccountIdentity(src.ID, "DANA@EXAMPLE.COM")
	require.NoError(err, "RemoveAccountIdentity")
	assert.Equal(int64(1), removed)
}
