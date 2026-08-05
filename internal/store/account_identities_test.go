package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRemoveAccountIdentityRecomputesOnlyIdentityDerivedAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	identityDerived := f.NewMessage().WithSourceMessageID("identity-derived").Build()
	identityDerived.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	identityDerivedID, err := st.UpsertMessage(identityDerived)
	require.NoError(err, "persist identity-derived candidate")

	sourceNative := f.NewMessage().
		WithSourceMessageID("source-native").
		WithIsFromMe(true).
		Build()
	sourceNative.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	sourceNativeID, err := st.UpsertMessage(sourceNative)
	require.NoError(err, "persist source-native message")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages SET source_is_from_me = NULL WHERE id = ?`),
		sourceNativeID,
	)
	require.NoError(err, "simulate legacy message without attribution provenance")
	_, err = st.DB().Exec(
		st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`),
		"message_attribution_provenance_v2",
	)
	require.NoError(err, "reset attribution migration sentinel")
	require.NoError(st.InitSchema(), "initialize legacy attribution provenance")

	require.NoError(st.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	afterAdd, err := st.GetMessageIsFromMe(identityDerivedID)
	require.NoError(err, "read identity-derived attribution after add")
	assert.True(afterAdd)

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "owner@example.com")
	require.NoError(err, "RemoveAccountIdentity")
	require.Equal(int64(1), removed)

	afterRemove, err := st.GetMessageIsFromMe(identityDerivedID)
	require.NoError(err, "read identity-derived attribution after remove")
	assert.False(afterRemove, "removing the last matching identity must clear derived attribution")
	nativeAfterRemove, err := st.GetMessageIsFromMe(sourceNativeID)
	require.NoError(err, "read source-native attribution after remove")
	assert.True(nativeAfterRemove, "identity removal must preserve source-native attribution")
}

func TestAddAccountIdentityUpdatesOnlyChangedMessageAttribution(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite audit trigger measures updated message rows")
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	ownerID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	otherID := f.EnsureParticipant("other@example.com", "Other", "example.com")

	ownerMessage := f.NewMessage().WithSourceMessageID("owner-message").Build()
	ownerMessage.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
	ownerMessageID, err := st.UpsertMessage(ownerMessage)
	require.NoError(err, "persist matching message")

	otherMessage := f.NewMessage().WithSourceMessageID("other-message").Build()
	otherMessage.SenderID = sql.NullInt64{Int64: otherID, Valid: true}
	otherMessageID, err := st.UpsertMessage(otherMessage)
	require.NoError(err, "persist unrelated message")

	_, err = st.DB().Exec(`
		DROP TRIGGER IF EXISTS trg_messages_last_modified;
		CREATE TABLE message_update_audit (message_id INTEGER NOT NULL);
		CREATE TRIGGER audit_message_update
		AFTER UPDATE ON messages
		BEGIN
			INSERT INTO message_update_audit (message_id) VALUES (NEW.id);
		END;
	`)
	require.NoError(err, "install message update audit")

	require.NoError(
		st.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"),
		"add matching identity",
	)

	var ownerUpdates, otherUpdates int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_update_audit WHERE message_id = ?`,
		ownerMessageID,
	).Scan(&ownerUpdates))
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_update_audit WHERE message_id = ?`,
		otherMessageID,
	).Scan(&otherUpdates))
	assert.Equal(1, ownerUpdates, "matching attribution must be updated")
	assert.Zero(otherUpdates, "unchanged attribution must not rewrite the message")
}

func TestUpsertMessageDerivesAttributionFromConfirmedIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(
		st.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"),
		"confirm identity before ingestion",
	)

	message := f.NewMessage().WithSourceMessageID("import-after-confirmation").Build()
	message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	messageID, err := st.UpsertMessage(message)
	require.NoError(err, "persist message")

	var isFromMe, sourceIsFromMe, identityIsFromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT is_from_me, source_is_from_me, identity_is_from_me
		FROM messages
		WHERE id = ?
	`), messageID).Scan(&isFromMe, &sourceIsFromMe, &identityIsFromMe))
	assert.True(isFromMe, "confirmed sender must be attributed during initial persistence")
	assert.False(sourceIsFromMe, "incoming message did not carry source-native attribution")
	assert.True(identityIsFromMe, "confirmed sender attribution must retain identity provenance")
}

func TestUpsertMessagePreservesRepairedIdentityAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	message := f.NewMessage().WithSourceMessageID("resynced-message").Build()
	message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	messageID, err := st.UpsertMessage(message)
	require.NoError(err, "persist unattributed message")

	require.NoError(
		st.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"),
		"confirm identity and repair existing message",
	)
	isFromMe, err := st.GetMessageIsFromMe(messageID)
	require.NoError(err, "read repaired attribution")
	require.True(isFromMe, "identity confirmation must repair the existing message")

	message.Subject = sql.NullString{String: "resynced subject", Valid: true}
	_, err = st.UpsertMessage(message)
	require.NoError(err, "re-upsert message without caller-derived attribution")

	var sourceIsFromMe, identityIsFromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT is_from_me, source_is_from_me, identity_is_from_me
		FROM messages
		WHERE id = ?
	`), messageID).Scan(&isFromMe, &sourceIsFromMe, &identityIsFromMe))
	assert.True(isFromMe, "re-sync must preserve attribution from the confirmed sender")
	assert.False(sourceIsFromMe, "re-sync did not introduce source-native attribution")
	assert.True(identityIsFromMe, "re-sync must preserve identity-derived provenance")
}

func TestAddAndListAccountIdentities(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity")

	ids, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 1)
	got := ids[0]
	assert.Equal("me@example.com", got.Address, "address")
	assert.Equal("manual", got.SourceSignal, "source_signal")
	assert.Equal(f.Source.ID, got.SourceID, "source_id")
	assert.False(got.ConfirmedAt.IsZero(), "confirmed_at should be set after first insert")
}

func TestAddAccountIdentity_Idempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity (1)")
	ids1, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities (1)")
	require.Len(ids1, 1, "after first insert")
	first := ids1[0].ConfirmedAt

	time.Sleep(2 * time.Millisecond)

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity (2)")
	ids2, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities (2)")
	assert.Len(ids2, 1, "after idempotent re-add")
	assert.True(ids2[0].ConfirmedAt.Equal(first),
		"confirmed_at moved on idempotent re-add: %v -> %v", first, ids2[0].ConfirmedAt)
}

// TestAddAccountIdentity_PreservesCase verifies that the first
// add of an email-shaped identifier wins the stored casing. Subsequent
// adds with different cases merge into the same row (case-insensitive
// match) rather than producing duplicate rows. This preserves the
// "case-preserved storage, email-case-insensitive logical identity"
// contract that the add/remove paths share.
func TestAddAccountIdentity_PreservesCase(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "Alice@Example.com", "manual"), "AddAccountIdentity Alice")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "AddAccountIdentity alice")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1, "want 1 row (email is case-insensitive)")
	assert.Equal(t, "Alice@Example.com", rows[0].Address,
		"address (case-preserved first-write)")
}

func TestAddAccountIdentity_AdditionalSignal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	rows1, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	first := rows1[0].ConfirmedAt
	time.Sleep(2 * time.Millisecond)

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "account-identifier"))
	rows2, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities after second signal")
	assert.Equal("account-identifier,manual", rows2[0].SourceSignal, "signal")
	assert.True(rows2[0].ConfirmedAt.Equal(first), "confirmed_at moved on signal augment")
}

func TestAddAccountIdentity_ThreeSignalAccumulation(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	for _, sig := range []string{"manual", "account-identifier", "is_from_me"} {
		require.NoError(t, st.AddAccountIdentity(f.Source.ID, "alice@example.com", sig))
	}
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(t, err, "ListAccountIdentities")
	assert.Equal(t, "account-identifier,is_from_me,manual", rows[0].SourceSignal, "signal")
}

func TestAddAccountIdentity_EmptySignalOnExistingRow(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Equal(t, "manual", rows[0].SourceSignal,
		"signal (empty signal on existing row should be no-op)")
}

func TestAddAccountIdentity_EmptySignalOnMissingRow(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1)
	require.Empty(rows[0].SourceSignal, "want one row with empty signal")
	assert.False(t, rows[0].ConfirmedAt.IsZero(), "confirmed_at should be set even with empty signal")
}

func TestAddAccountIdentity_NonEmptySignalReplacesEmptyRow(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", ""))
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Equal(t, "manual", rows[0].SourceSignal, "signal")
}

func TestAddAccountIdentity_RejectsCommaInSignal(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	err := st.AddAccountIdentity(f.Source.ID, "alice@example.com", "a,b")
	require.Error(t, err, "expected error for comma in signal")
	assert.ErrorContains(t, err, "comma")
}

func TestAddAccountIdentity_AllWhitespaceIdentifierIsNoOp(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	require.NoError(t, st.AddAccountIdentity(f.Source.ID, "   ", "manual"))
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(t, err, "ListAccountIdentities")
	assert.Empty(t, rows, "whitespace identifier should not insert")
}

func TestAccountIdentities_FKCascadeOnSourceDelete(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	require.NoError(st.RemoveSource(f.Source.ID))
	var n int
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM account_identities WHERE source_id = ?`), f.Source.ID,
	).Scan(&n))
	assert.Equal(t, 0, n, "FK cascade failed: %d rows remain", n)
}

func TestGetIdentitiesForScope_MultiSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	src2, err := st.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "GetOrCreateSource")

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "add alice")
	require.NoError(st.AddAccountIdentity(src2.ID, "bob@example.com", "manual"), "add bob")

	scope, err := st.GetIdentitiesForScope([]int64{f.Source.ID, src2.ID})
	require.NoError(err, "GetIdentitiesForScope")

	require.Len(scope, 2)
	assert.Contains(scope, "alice@example.com")
	assert.Contains(scope, "bob@example.com")
}

func TestGetIdentitiesForScope_EmptyInput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "add identity")

	scope, err := st.GetIdentitiesForScope([]int64{})
	require.NoError(err, "GetIdentitiesForScope empty")
	assert.NotNil(scope, "expected non-nil map for empty scope")
	assert.Empty(scope, "want empty scope")
}

// TestAddAccountIdentity_BumpsIdentityRevisionOnNewIdentity verifies that
// confirming a brand new (source_id, address) pair bumps both the identity
// revision (since it changes which participants are owners for the source —
// the owner_participants cache dataset) and the account-identity revision
// (since it changes the message-baked is_from_me derivation, which only a
// full cache rebuild can re-derive).
func TestAddAccountIdentity_BumpsIdentityRevisionOnNewIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	before, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision before")
	acctBefore, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision before")

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))

	after, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after")
	assert.Equal(before+1, after, "adding a new identity should bump the identity revision")
	acctAfter, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after")
	assert.Equal(acctBefore+1, acctAfter, "adding a new identity should bump the account identity revision")
}

// TestAddAccountIdentity_DuplicateAddDoesNotBumpRevision guards
// idempotency: re-adding the exact same (source_id, address, signal) is a
// no-op for owner_participants, so it must not bump either revision.
func TestAddAccountIdentity_DuplicateAddDoesNotBumpRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	after1, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after first add")
	acctAfter1, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after first add")

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	after2, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after duplicate add")
	assert.Equal(after1, after2, "re-adding the same identity should not bump the identity revision")
	acctAfter2, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after duplicate add")
	assert.Equal(acctAfter1, acctAfter2, "re-adding the same identity should not bump the account identity revision")
}

// TestAddAccountIdentity_NewSignalOnExistingAddressDoesNotBumpRevision
// guards that merging a new signal into an already-confirmed address does
// not bump either revision: the (source_id, address) mapping that
// owner_participants derives from is unchanged, only the evidence trail is.
func TestAddAccountIdentity_NewSignalOnExistingAddressDoesNotBumpRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	after1, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after first add")
	acctAfter1, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after first add")

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "account-identifier"))
	after2, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after signal augment")
	assert.Equal(after1, after2, "adding a new signal to an existing address should not bump the identity revision")
	acctAfter2, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after signal augment")
	assert.Equal(acctAfter1, acctAfter2,
		"adding a new signal to an existing address should not bump the account identity revision")
}

// TestRemoveAccountIdentity_BumpsIdentityRevisionOnHit verifies that
// removing a confirmed identity bumps both the identity revision and the
// account-identity revision, since it changes which participants are
// owners for the source and invalidates the message-baked is_from_me flag.
func TestRemoveAccountIdentity_BumpsIdentityRevisionOnHit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"))
	before, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision before remove")
	acctBefore, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision before remove")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "alice@example.com")
	require.NoError(err, "RemoveAccountIdentity")
	require.Equal(int64(1), removed, "removed")

	after, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after remove")
	assert.Equal(before+1, after, "removing an existing identity should bump the identity revision")
	acctAfter, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after remove")
	assert.Equal(acctBefore+1, acctAfter, "removing an existing identity should bump the account identity revision")
}

// TestRemoveAccountIdentity_MissDoesNotBumpRevision guards idempotency:
// removing an identity that does not exist must not bump either revision.
func TestRemoveAccountIdentity_MissDoesNotBumpRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	before, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision before")
	acctBefore, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision before")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "nope@example.com")
	require.NoError(err, "RemoveAccountIdentity")
	assert.Equal(int64(0), removed, "removed on miss")

	after, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision after")
	assert.Equal(before, after, "removing a missing identity should not bump the identity revision")
	acctAfter, err := st.AccountIdentityRevision()
	require.NoError(err, "AccountIdentityRevision after")
	assert.Equal(acctBefore, acctAfter, "removing a missing identity should not bump the account identity revision")
}

func TestRemoveAccountIdentity_Hit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"), "add identity")
	removed, err := st.RemoveAccountIdentity(f.Source.ID, "alice@example.com")
	require.NoError(err, "RemoveAccountIdentity")
	assert.Equal(int64(1), removed, "removed")
	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Empty(rows)
}

func TestRemoveAccountIdentity_Miss(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "nope@example.com")
	require.NoError(t, err, "RemoveAccountIdentity")
	assert.Equal(t, int64(0), removed, "removed on miss")
}

// TestRemoveAccountIdentity_EmailIsCaseInsensitive verifies that an
// email-shaped identifier removed with different casing matches the
// stored row, since email addresses are case-insensitive in practice.
func TestRemoveAccountIdentity_EmailIsCaseInsensitive(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@Example.com", "manual"),
		"add identity")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "ALICE@example.com")
	require.NoError(err, "RemoveAccountIdentity")
	require.Equal(int64(1), removed, "removed (email match should be case-insensitive)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	assert.Empty(t, rows)
}

// TestAddAccountIdentity_EmailIsCaseInsensitive verifies that a second
// add with different casing merges signals into the existing row
// instead of inserting a duplicate. This pairs with
// TestRemoveAccountIdentity_EmailIsCaseInsensitive: add/remove must
// agree on case-folding for "@"-shaped identifiers, otherwise an
// 'identity add Foo@x.com' followed by 'identity remove foo@x.com'
// could leave (or remove) the wrong row.
func TestAddAccountIdentity_EmailIsCaseInsensitive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alice@example.com", "manual"),
		"first add (lowercase)")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "ALICE@Example.com", "is_from_me"),
		"second add (different case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 1, "want case-folded merge")
	assert.Equal("alice@example.com", rows[0].Address, "first-write")
	assert.Contains(rows[0].SourceSignal, "manual", "source_signal merged")
	assert.Contains(rows[0].SourceSignal, "is_from_me", "source_signal merged")
}

// TestAddAccountIdentity_NonEmailStaysCaseSensitive guards the
// chat-handle invariant: synthetic identifiers can be case-significant
// so two distinct cases must produce two rows.
func TestAddAccountIdentity_NonEmailStaysCaseSensitive(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "AliceHandle", "manual"),
		"first add")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "alicehandle", "manual"),
		"second add (different case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 2, "want 2 distinct rows for non-email")
}

// TestAddAccountIdentity_MatrixMXIDStaysCaseSensitive guards against an
// over-broad email heuristic: Matrix MXIDs like "@user:server.org" start
// with "@" and contain a "." but are not emails. Two distinct cases must
// produce two distinct rows.
func TestAddAccountIdentity_MatrixMXIDStaysCaseSensitive(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "@Alice:matrix.org", "manual"),
		"first add (Matrix MXID, mixed case)")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "@alice:matrix.org", "manual"),
		"second add (Matrix MXID, lower case)")

	rows, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(rows, 2, "want 2 distinct rows for Matrix MXID")
}

// TestRemoveAccountIdentity_NonEmailIsCaseSensitive guards the
// case-preserving path for synthetic identifiers (chat handles, etc.):
// removing with different casing on a non-email value must not match.
func TestRemoveAccountIdentity_NonEmailIsCaseSensitive(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	require.NoError(t,
		st.AddAccountIdentity(f.Source.ID, "AliceHandle", "manual"),
		"add identity")

	removed, err := st.RemoveAccountIdentity(f.Source.ID, "alicehandle")
	require.NoError(t, err, "RemoveAccountIdentity")
	require.Equal(t, int64(0), removed, "removed on case-mismatch for non-email identifier")
}

// TestMergeConfirmedAccountIdentitySignalsSkipsUnconfirmedAddresses pins the
// merge-only write boundary: refresh paths pass candidates they believe are
// confirmed, and the store must refuse to create ownership for any address
// whose row is absent.
func TestMergeConfirmedAccountIdentitySignalsSkipsUnconfirmedAddresses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "Owner@Example.test", "manual"), "confirm identity")
	beforeIdentityRevision, err := st.IdentityRevision()
	require.NoError(err)
	beforeAccountRevision, err := st.AccountIdentityRevision()
	require.NoError(err)

	outcomes, err := st.MergeConfirmedAccountIdentitySignalsContext(
		t.Context(), f.Source.ID, []store.IdentityConfirmation{
			{Identifier: "owner@example.test", Signals: []string{"sent-label"}},
			{Identifier: "stranger@example.test", Signals: []string{"is_from_me", "sent-label"}},
		})
	require.NoError(err)
	assert.Equal([]store.IdentityConfirmationOutcome{{
		Identifier: "owner@example.test", Added: false, Signals: []string{"sent-label"},
	}}, outcomes, "only the already-confirmed identity may merge")

	identities, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err)
	require.Len(identities, 1, "a merge must never create an ownership row")
	assert.Equal("Owner@Example.test", identities[0].Address, "existing spelling wins case folding")
	assert.Equal("manual,sent-label", identities[0].SourceSignal, "confirmed identity still merges signals")

	afterIdentityRevision, err := st.IdentityRevision()
	require.NoError(err)
	afterAccountRevision, err := st.AccountIdentityRevision()
	require.NoError(err)
	assert.Equal(beforeIdentityRevision, afterIdentityRevision, "signal-only merge must not bump")
	assert.Equal(beforeAccountRevision, afterAccountRevision, "signal-only merge must not bump")
}

// TestMergeConfirmedAccountIdentitySignalsDoesNotResurrectRemovedIdentity
// covers the race the merge-only boundary exists for: a refresh reads the
// confirmed set, the identity is removed, and the stale write arrives after.
func TestMergeConfirmedAccountIdentitySignalsDoesNotResurrectRemovedIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "retired@example.test", "manual"), "confirm identity")
	stale := []store.IdentityConfirmation{{Identifier: "retired@example.test", Signals: []string{"sent-label"}}}
	removed, err := st.RemoveAccountIdentity(f.Source.ID, "retired@example.test")
	require.NoError(err, "remove identity after the refresh read its candidate set")
	require.Equal(int64(1), removed)

	outcomes, err := st.MergeConfirmedAccountIdentitySignalsContext(t.Context(), f.Source.ID, stale)
	require.NoError(err)
	assert.Empty(outcomes)

	identities, err := st.ListAccountIdentities(f.Source.ID)
	require.NoError(err)
	assert.Empty(identities, "a stale refresh write must not resurrect a removed identity")
}
