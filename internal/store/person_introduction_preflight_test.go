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
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "account-a")
	req.NoError(err)
	pa := f.EnsureParticipant("a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("b@example.com", "B", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("account-a", "alice"), pb, beeperidentity.Fallback("account-b", "bob"))
	req.NoError(err)
	p, err := f.Store.GetPersonIntroductionPreflightContext(ctx, source.ID, a.ID, b.ID)
	req.NoError(err)
	req.Len(p.Members, 1)
	check.Equal("alice", p.Members[0].UserID)
	req.Len(p.Blockers, 1)
	check.Equal("no_identity_on_account", p.Blockers[0].Code)
	check.Equal(b.ID, p.Blockers[0].PersonID)
}

func TestIntroductionPreflightAmbiguityAndOwner(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "account-a")
	req.NoError(err)
	other, err := f.Store.GetOrCreateSource("beeper", "account-b")
	req.NoError(err)
	pa := f.EnsureParticipant("member-a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("member-b@example.com", "B", "example.com")
	pc := f.EnsureParticipant("candidate@example.com", "C", "example.com")
	pd := f.EnsureParticipant("candidate2@example.com", "D", "example.com")
	_, err = f.Store.LinkParticipants(pa, pb)
	req.NoError(err)
	_, err = f.Store.LinkParticipants(pc, pd)
	req.NoError(err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	req.NoError(err)
	// The account address matches only the scoped account-B identifier, not an email.
	req.NoError(f.Store.AddAccountIdentityContext(ctx, other.ID, "self-opaque", "manual"))
	for _, entry := range []struct {
		id            int64
		account, user string
	}{
		{pa, "account-a", "member"}, {pb, "account-b", "self-opaque"},
		{pc, "account-a", "one"}, {pd, "account-a", "two"},
	} {
		_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), entry.id, beeperidentity.Fallback(entry.account, entry.user))
		req.NoError(err)
	}
	got, err := f.Store.GetPersonIntroductionPreflightContext(ctx, source.ID, a.ID, b.ID)
	req.NoError(err)
	check.Contains(got.Blockers, store.IntroductionBlocker{Code: "owner_selected", PersonID: a.ID})
	check.Contains(got.Blockers, store.IntroductionBlocker{Code: "ambiguous_identity", PersonID: b.ID})
	req.Len(got.Members, 1)
	check.Equal(a.ID, got.Members[0].PersonID)
	check.Equal("member", got.Members[0].UserID)
}

func TestIntroductionListPreflightEligibilityParity(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "parity")
	require.NoError(t, err)
	makePerson := func(name, account string) int64 {
		t.Helper()
		id := f.EnsureParticipant(name+"@example.test", name, "example.test")
		p, _, err := f.Store.CreatePersonFromParticipantContext(ctx, id)
		require.NoError(t, err)
		if account != "" {
			_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), id, beeperidentity.Fallback(account, name))
			require.NoError(t, err)
		}
		return p.ID
	}
	a := makePerson("parity-a", "parity")
	b := makePerson("parity-b", "parity")
	missing := makePerson("parity-missing", "")
	cross := makePerson("parity-cross", "other")
	owner := makePerson("parity-owner", "parity")
	require.NoError(t, f.Store.AddAccountIdentityContext(ctx, src.ID, "parity-owner@example.test", "manual"))
	for _, person := range []int64{b, missing, cross, owner} {
		pre, err := f.Store.GetPersonIntroductionPreflightContext(ctx, src.ID, a, person)
		require.NoError(t, err)
		row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a, PersonBID: person}, "owner")
		require.NoError(t, err)
		page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, []int64{src.ID})
		require.NoError(t, err)
		visible := true
		for _, blocker := range pre.Blockers {
			if blocker.Code == "owner_selected" || blocker.Code == "no_identity_on_account" {
				visible = false
			}
		}
		found := false
		for _, item := range page.Items {
			found = found || item.IntroID == row.IntroID
		}
		assert.Equal(t, visible, found)
	}
}
