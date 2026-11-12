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
	source, err := f.Store.GetOrCreateSource("beeper", "owner-account")
	require.NoError(t, err)
	pa := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	pb := f.EnsureParticipant("other@example.com", "Other", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentityContext(ctx, source.ID, "owner@example.com", "manual"))
	for _, entry := range []struct {
		id  int64
		key string
	}{{pa, beeperidentity.Fallback("owner-account", "owner")}, {pb, beeperidentity.Fallback("owner-account", "one")}, {pb, beeperidentity.Fallback("owner-account", "two")}} {
		_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), entry.id, entry.key)
		require.NoError(t, err)
	}
	got, err := f.Store.GetPersonIntroductionPreflightContext(ctx, source.ID, a.ID, b.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Blockers, store.IntroductionBlocker{Code: "owner_selected", PersonID: a.ID})
	assert.Contains(t, got.Blockers, store.IntroductionBlocker{Code: "ambiguous_identity", PersonID: b.ID})
}

func TestIntroductionPreflightMultipleParticipants(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "account")
	require.NoError(t, err)
	p1 := f.EnsureParticipant("candidate1@example.com", "One", "example.com")
	p2 := f.EnsureParticipant("candidate2@example.com", "Two", "example.com")
	p3 := f.EnsureParticipant("candidate3@example.com", "Three", "example.com")
	_, err = f.Store.LinkParticipants(p1, p2)
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, p1)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, p3)
	require.NoError(t, err)
	for _, entry := range []struct {
		id   int64
		user string
	}{{p1, "one"}, {p2, "two"}, {p3, "three"}} {
		_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value) VALUES (?, 'beeper', ?)`), entry.id, beeperidentity.Fallback("account", entry.user))
		require.NoError(t, err)
	}
	result, err := f.Store.GetPersonIntroductionPreflightContext(ctx, src.ID, a.ID, b.ID)
	require.NoError(t, err)
	assert.Contains(t, result.Blockers, store.IntroductionBlocker{Code: "ambiguous_identity", PersonID: a.ID})
	require.Len(t, result.Members, 1)
	assert.Equal(t, b.ID, result.Members[0].PersonID)
}
