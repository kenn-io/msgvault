package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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

// TestAccountIdentityKeyRepairRunsAfterProvenanceInitialization proves the
// InitSchema ordering contract: on a pre-provenance archive, the duplicate
// collapse's attribution refresh must not run before source_is_from_me is
// initialized, or the provenance backfill would read identity-derived
// is_from_me values as source-native and bake them in permanently.
func TestAccountIdentityKeyRepairRunsAfterProvenanceInitialization(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	senderID := f.EnsureParticipant("owner2@example.com", "Owner Two", "example.com")

	src, err := st.GetOrCreateSource("gmail", "provenance-order@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "provenance-order-conversation", "Thread")
	require.NoError(err, "EnsureConversation")
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		ConversationID:  convID,
		SourceMessageID: "provenance-order-message",
		SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
		IsFromMe:        false,
	})
	require.NoError(err, "persist received message")

	// Case-variant duplicate identities written before the key column, plus
	// a message predating attribution provenance: the archive state an
	// upgrade encounters.
	for _, addr := range []string{"Owner2@Example.com", "owner2@example.com"} {
		_, err = st.DB().Exec(st.Rebind(`
			INSERT INTO account_identities (source_id, address, source_signal)
			VALUES (?, ?, ?)`), src.ID, addr, "manual")
		require.NoError(err, "seed legacy identity %s", addr)
	}
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages
		SET source_is_from_me = NULL, identity_is_from_me = FALSE
		WHERE id = ?`), messageID)
	require.NoError(err, "simulate pre-provenance message row")
	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM applied_migrations WHERE name = ?`),
		"message_attribution_provenance_v3")
	require.NoError(err, "reset attribution migration sentinel")

	require.NoError(st.InitSchema(), "run production schema migration")

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1, "duplicates must collapse during the same InitSchema")

	var sourceDerived sql.NullBool
	var identityDerived bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT source_is_from_me, identity_is_from_me
		FROM messages WHERE id = ?`), messageID).Scan(&sourceDerived, &identityDerived))
	assert.True(sourceDerived.Valid, "provenance must be initialized")
	assert.False(sourceDerived.Bool,
		"identity-derived attribution must not be recorded as source-native")
	assert.True(identityDerived, "sender matches a confirmed identity")
	isFromMe, err := st.GetMessageIsFromMe(messageID)
	require.NoError(err, "GetMessageIsFromMe")
	assert.True(isFromMe, "effective attribution comes from the identity match")
}
