package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func readRevisions(t *testing.T, f *storetest.Fixture) (identity, account, identifier int64) {
	t.Helper()
	identity, err := f.Store.IdentityRevision()
	require.NoError(t, err, "IdentityRevision")
	account, err = f.Store.AccountIdentityRevision()
	require.NoError(t, err, "AccountIdentityRevision")
	identifier, err = f.Store.ParticipantIdentifierRevision()
	require.NoError(t, err, "ParticipantIdentifierRevision")
	return identity, account, identifier
}

// TestSetParticipantIdentifierOwnerEvidenceBumpsRevisions pins the cache
// staleness contract: pointing an identifier that matches a confirmed
// account-identity address at a participant changes owner_participants and
// the export-derived is_from_me/is_owner bakes, so the mutation must bump
// both revisions (the account-identity bump forces the full rebuild that
// re-derives committed shards). Idempotent re-runs must bump nothing, or
// every importer re-run would force a rebuild.
func TestSetParticipantIdentifierOwnerEvidenceBumpsRevisions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "Me@Example.com", "manual"), "AddAccountIdentity")
	alias := f.EnsureParticipant("alias@example.com", "Alias", "example.com")

	identityBefore, accountBefore, identifierBefore := readRevisions(t, f)
	require.NoError(st.SetParticipantIdentifier(alias, "email", "me@example.com"),
		"set owner-evidence identifier")
	identityAfter, accountAfter, identifierAfter := readRevisions(t, f)
	assert.Greater(identityAfter, identityBefore, "identity revision after owner-evidence change")
	assert.Greater(accountAfter, accountBefore, "account identity revision after owner-evidence change")
	assert.Greater(identifierAfter, identifierBefore, "participant identifier revision after change")

	require.NoError(st.SetParticipantIdentifier(alias, "email", "me@example.com"),
		"idempotent re-set")
	identityNoop, accountNoop, identifierNoop := readRevisions(t, f)
	assert.Equal(identityAfter, identityNoop, "identity revision after no-op re-set")
	assert.Equal(accountAfter, accountNoop, "account identity revision after no-op re-set")
	assert.Equal(identifierAfter, identifierNoop, "participant identifier revision after no-op re-set")
}

// TestSetParticipantIdentifierRepointOwnerEvidenceBumpsRevisions covers the
// repoint direction: moving owner evidence from one participant to another
// changes which participant resolves as the owner, so it must bump both
// revisions even though no new row is created.
func TestSetParticipantIdentifierRepointOwnerEvidenceBumpsRevisions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "+15550100", "manual"), "AddAccountIdentity")
	first := f.EnsureParticipant("first@example.com", "First", "example.com")
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(st.SetParticipantIdentifier(first, "phone", "+15550100"), "seed owner evidence")

	identityBefore, accountBefore, identifierBefore := readRevisions(t, f)
	require.NoError(st.SetParticipantIdentifier(second, "phone", "+15550100"), "repoint owner evidence")
	identityAfter, accountAfter, identifierAfter := readRevisions(t, f)
	assert.Greater(identityAfter, identityBefore, "identity revision after owner-evidence repoint")
	assert.Greater(accountAfter, accountBefore, "account identity revision after owner-evidence repoint")
	assert.Greater(identifierAfter, identifierBefore, "participant identifier revision after repoint")

	id, _, err := st.ParticipantByIdentifier("phone", "+15550100")
	require.NoError(err, "ParticipantByIdentifier")
	assert.Equal(second, id, "identifier must point at the new participant")
}

func TestSetParticipantIdentifierRepointRefreshesSenderAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "+15550100", "manual"), "confirm owner identity")
	first := f.EnsureParticipant("first@example.com", "First", "example.com")
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(st.SetParticipantIdentifier(first, "phone", "+15550100"), "seed owner evidence")

	persistMessage := func(sourceMessageID string, senderID int64) int64 {
		message := f.NewMessage().WithSourceMessageID(sourceMessageID).Build()
		message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		messageID, err := st.UpsertMessage(message)
		require.NoError(err, "persist %s", sourceMessageID)
		return messageID
	}
	firstMessageID := persistMessage("identifier-first", first)
	secondMessageID := persistMessage("identifier-second", second)

	require.NoError(st.SetParticipantIdentifier(second, "phone", "+15550100"), "repoint owner evidence")

	firstIsFromMe, err := st.GetMessageIsFromMe(firstMessageID)
	require.NoError(err, "read former owner attribution")
	assert.False(firstIsFromMe, "reassignment must clear attribution from the former identifier owner")
	secondIsFromMe, err := st.GetMessageIsFromMe(secondMessageID)
	require.NoError(err, "read new owner attribution")
	assert.True(secondIsFromMe, "reassignment must attribute messages from the new identifier owner")

	var firstIdentityDerived, secondIdentityDerived bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT identity_is_from_me
		FROM messages
		WHERE id = ?
	`), firstMessageID).Scan(&firstIdentityDerived))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT identity_is_from_me
		FROM messages
		WHERE id = ?
	`), secondMessageID).Scan(&secondIdentityDerived))
	assert.False(firstIdentityDerived, "former identifier owner provenance")
	assert.True(secondIdentityDerived, "new identifier owner provenance")
}

// TestSetParticipantIdentifierNonOwnerEvidenceBumpsOnlyIdentifierRevision
// verifies plain alternate identifiers (the common importer case) advance
// only the participant-identifier revision — the identity directory bakes
// identifier values, so the derived refresh must fire, but neither the
// identity nor account-identity revision moves (no full rebuild, and no
// escalation when new messages coincide).
func TestSetParticipantIdentifierNonOwnerEvidenceBumpsOnlyIdentifierRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity")
	alias := f.EnsureParticipant("alias@example.com", "Alias", "example.com")

	identityBefore, accountBefore, identifierBefore := readRevisions(t, f)
	require.NoError(st.SetParticipantIdentifier(alias, "email", "other@example.com"),
		"set non-owner identifier")
	identityAfter, accountAfter, identifierAfter := readRevisions(t, f)
	assert.Equal(identityBefore, identityAfter, "identity revision after non-owner identifier")
	assert.Equal(accountBefore, accountAfter, "account identity revision after non-owner identifier")
	assert.Greater(identifierAfter, identifierBefore, "participant identifier revision after non-owner identifier")

	id, _, err := st.ParticipantByIdentifier("email", "other@example.com")
	require.NoError(err, "ParticipantByIdentifier")
	assert.Equal(alias, id, "identifier mapping must still be written")
}
