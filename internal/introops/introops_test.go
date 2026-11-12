package introops_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestGrantBodyAndEdit(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "test-account")
	req.NoError(err)
	pa := f.EnsureParticipant("a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("b@example.com", "B", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("test-account", "a"), pb, beeperidentity.Fallback("test-account", "b"))
	req.NoError(err)
	svc := introops.Service{Store: f.Store}
	grant := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "test-account"}}}
	row, err := svc.Prepare(ctx, grant, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID, Body: "private"})
	req.NoError(err)
	check.Empty(row.Body)
	check.Equal("agent", row.CreatedBy)
	title := "changed"
	_, err = svc.Edit(ctx, grant, row.IntroID, store.PersonIntroductionEdit{Revision: 1, Title: &title})
	req.ErrorIs(err, introops.ErrForbidden)
	grant.Permissions = append(grant.Permissions, agentgrant.PermissionDraftEdit)
	row, err = svc.Get(ctx, grant, row.IntroID)
	req.NoError(err)
	check.Equal("private", row.Body)
	grant.Sources = nil
	_, err = svc.Get(ctx, grant, row.IntroID)
	req.ErrorIs(err, introops.ErrForbidden)
	_, err = svc.Get(ctx, grant, "missing")
	req.ErrorIs(err, introops.ErrForbidden)
}

func TestListFiltersSourcesBeforePagination(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	allowed, err := f.Store.GetOrCreateSource("beeper", "allowed")
	req.NoError(err)
	other, err := f.Store.GetOrCreateSource("beeper", "other")
	req.NoError(err)
	pa := f.EnsureParticipant("list-a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("list-b@example.com", "B", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("allowed", "a"), pb, beeperidentity.Fallback("allowed", "b"))
	req.NoError(err)
	for _, id := range []int64{allowed.ID, other.ID} {
		_, err = f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: id, PersonAID: a.ID, PersonBID: b.ID, Body: "hidden"}, "owner")
		req.NoError(err)
	}
	g := &agentgrant.Grant{ID: "reader", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: 999, Type: "beeper", Identifier: "allowed"}}}
	svc := introops.Service{Store: f.Store}
	first, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	req.Len(first.Items, 1)
	check.Equal(allowed.ID, first.Items[0].SourceID)
	check.Empty(first.Items[0].Body)
	check.False(first.HasMore)
}

func TestPrepareRejectsNonpositiveSourceID(t *testing.T) {
	f := storetest.New(t)
	svc := introops.Service{Store: f.Store}
	for _, id := range []int64{0, -1} {
		_, err := svc.Prepare(t.Context(), nil, store.PersonIntroductionPrepare{SourceID: id})
		require.ErrorIs(t, err, store.ErrIntroductionInvalid)
		assert.Equal(t, "invalid_args", introops.ErrorCode(err))
	}
}

func TestDelegatedPersonVisibility(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "scope-a")
	req.NoError(err)
	other, err := f.Store.GetOrCreateSource("beeper", "scope-b")
	req.NoError(err)
	person := func(name, account string) int64 {
		participant := f.EnsureParticipant(name+"@example.test", name, "example.test")
		p, _, err := f.Store.CreatePersonFromParticipantContext(ctx, participant)
		req.NoError(err)
		if account != "" {
			_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), participant, beeperidentity.Fallback(account, name))
			req.NoError(err)
		}
		return p.ID
	}
	a, b := person("a", "scope-a"), person("b", "scope-a")
	wrong, absent, owner := person("wrong", "scope-b"), person("absent", ""), person("self", "scope-a")
	req.NoError(f.Store.AddAccountIdentityContext(ctx, other.ID, "self@example.test", "manual"))
	svc := introops.Service{Store: f.Store}
	g := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: other.ID, Type: "beeper", Identifier: "scope-a"}}}
	in := store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a, PersonBID: b, Body: "private"}
	visible, err := svc.Prepare(ctx, g, in)
	req.NoError(err)
	check.Empty(visible.Body)
	for _, id := range []int64{wrong, absent, owner, 99999999} {
		in.PersonBID = id
		_, err := svc.Prepare(ctx, g, in)
		req.ErrorIs(err, introops.ErrForbidden)
		check.Equal("not_permitted", introops.ErrorCode(err))
	}
	// Owners can still prepare without an identity on the chosen account.
	in.PersonBID = absent
	hidden, err := svc.Prepare(ctx, nil, in)
	req.NoError(err)
	_, err = svc.Get(ctx, g, hidden.IntroID)
	req.ErrorIs(err, introops.ErrForbidden)
	_, err = svc.Cancel(ctx, g, hidden.IntroID, hidden.Revision)
	req.ErrorIs(err, introops.ErrForbidden)
	for _, id := range []int64{wrong, owner} {
		in.PersonBID = id
		row, err := svc.Prepare(ctx, nil, in)
		req.NoError(err)
		_, err = svc.Get(ctx, g, row.IntroID)
		req.ErrorIs(err, introops.ErrForbidden)
		_, err = svc.Cancel(ctx, g, row.IntroID, row.Revision)
		req.ErrorIs(err, introops.ErrForbidden)
	}
	in.PersonBID = b
	second, err := svc.Prepare(ctx, nil, in)
	req.NoError(err)
	firstPage, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	req.Len(firstPage.Items, 1)
	check.True(firstPage.HasMore)
	secondPage, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1, AfterID: firstPage.NextAfterID})
	req.NoError(err)
	req.Len(secondPage.Items, 1)
	check.False(secondPage.HasMore)
	check.ElementsMatch([]string{visible.IntroID, second.IntroID}, []string{firstPage.Items[0].IntroID, secondPage.Items[0].IntroID})
	all, err := svc.List(ctx, nil, store.PersonIntroductionQuery{})
	req.NoError(err)
	check.Len(all.Items, 5)
}
