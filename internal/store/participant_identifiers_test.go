package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func readRevisions(t *testing.T, f *storetest.Fixture) (identity, account int64) {
	t.Helper()
	identity, err := f.Store.IdentityRevision()
	require.NoError(t, err, "IdentityRevision")
	account, err = f.Store.AccountIdentityRevision()
	require.NoError(t, err, "AccountIdentityRevision")
	return identity, account
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

	identityBefore, accountBefore := readRevisions(t, f)
	require.NoError(st.SetParticipantIdentifier(alias, "email", "me@example.com"),
		"set owner-evidence identifier")
	identityAfter, accountAfter := readRevisions(t, f)
	assert.Greater(identityAfter, identityBefore, "identity revision after owner-evidence change")
	assert.Greater(accountAfter, accountBefore, "account identity revision after owner-evidence change")

	require.NoError(st.SetParticipantIdentifier(alias, "email", "me@example.com"),
		"idempotent re-set")
	identityNoop, accountNoop := readRevisions(t, f)
	assert.Equal(identityAfter, identityNoop, "identity revision after no-op re-set")
	assert.Equal(accountAfter, accountNoop, "account identity revision after no-op re-set")
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

	identityBefore, accountBefore := readRevisions(t, f)
	require.NoError(st.SetParticipantIdentifier(second, "phone", "+15550100"), "repoint owner evidence")
	identityAfter, accountAfter := readRevisions(t, f)
	assert.Greater(identityAfter, identityBefore, "identity revision after owner-evidence repoint")
	assert.Greater(accountAfter, accountBefore, "account identity revision after owner-evidence repoint")

	id, _, err := st.ParticipantByIdentifier("phone", "+15550100")
	require.NoError(err, "ParticipantByIdentifier")
	assert.Equal(second, id, "identifier must point at the new participant")
}

// TestSetParticipantIdentifierNonOwnerEvidenceDoesNotBump verifies plain
// alternate identifiers (the common importer case) still write without
// churning cache revisions.
func TestSetParticipantIdentifierNonOwnerEvidenceDoesNotBump(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity")
	alias := f.EnsureParticipant("alias@example.com", "Alias", "example.com")

	identityBefore, accountBefore := readRevisions(t, f)
	require.NoError(st.SetParticipantIdentifier(alias, "email", "other@example.com"),
		"set non-owner identifier")
	identityAfter, accountAfter := readRevisions(t, f)
	assert.Equal(identityBefore, identityAfter, "identity revision after non-owner identifier")
	assert.Equal(accountBefore, accountAfter, "account identity revision after non-owner identifier")

	id, _, err := st.ParticipantByIdentifier("email", "other@example.com")
	require.NoError(err, "ParticipantByIdentifier")
	assert.Equal(alias, id, "identifier mapping must still be written")
}
