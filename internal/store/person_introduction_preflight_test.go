package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIntroductionPreflightScopedIdentity(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "account-a")
	require.NoError(t, err)
	pa := f.EnsureParticipant("a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("b@example.com", "B", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("account-a", "alice"), pb, beeperidentity.Fallback("account-b", "bob"))
	require.NoError(t, err)
	p, err := f.Store.GetPersonIntroductionPreflightContext(ctx, source.ID, a.ID, b.ID)
	require.NoError(t, err)
	require.Len(t, p.Members, 1)
	assert.Equal(t, "alice", p.Members[0].UserID)
	require.Len(t, p.Blockers, 1)
	assert.Equal(t, "no_identity_on_account", p.Blockers[0].Code)
	assert.Equal(t, b.ID, p.Blockers[0].PersonID)
}

func TestIntroductionPreflightAmbiguityAndOwner(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "account-a")
	require.NoError(t, err)
	other, err := f.Store.GetOrCreateSource("beeper", "account-b")
	require.NoError(t, err)
	pa := f.EnsureParticipant("member-a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("member-b@example.com", "B", "example.com")
	pc := f.EnsureParticipant("candidate@example.com", "C", "example.com")
	pd := f.EnsureParticipant("candidate2@example.com", "D", "example.com")
	_, err = f.Store.LinkParticipants(pa, pb)
	require.NoError(t, err)
	_, err = f.Store.LinkParticipants(pc, pd)
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	require.NoError(t, err)
	// The account address matches only the scoped account-B identifier, not an email.
	require.NoError(t, f.Store.AddAccountIdentityContext(ctx, other.ID, "self-opaque", "manual"))
	for _, entry := range []struct {
		id            int64
		account, user string
	}{
		{pa, "account-a", "member"}, {pb, "account-b", "self-opaque"},
		{pc, "account-a", "one"}, {pd, "account-a", "two"},
	} {
		_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), entry.id, beeperidentity.Fallback(entry.account, entry.user))
		require.NoError(t, err)
	}
	got, err := f.Store.GetPersonIntroductionPreflightContext(ctx, source.ID, a.ID, b.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Blockers, store.IntroductionBlocker{Code: "owner_selected", PersonID: a.ID})
	assert.Contains(t, got.Blockers, store.IntroductionBlocker{Code: "ambiguous_identity", PersonID: b.ID})
	require.Len(t, got.Members, 1)
	assert.Equal(t, a.ID, got.Members[0].PersonID)
	assert.Equal(t, "member", got.Members[0].UserID)
}
