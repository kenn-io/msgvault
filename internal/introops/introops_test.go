package introops_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestGrantBodyAndEdit(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "test-account")
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("a@example.com", "A", "example.com"))
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("b@example.com", "B", "example.com"))
	require.NoError(t, err)
	svc := introops.Service{Store: f.Store}
	grant := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "test-account"}}}
	row, err := svc.Prepare(ctx, grant, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID, Body: "private"})
	require.NoError(t, err)
	assert.Empty(t, row.Body)
	assert.Equal(t, "agent", row.CreatedBy)
	title := "changed"
	_, err = svc.Edit(ctx, grant, row.IntroID, store.PersonIntroductionEdit{Revision: 1, Title: &title})
	assert.ErrorIs(t, err, introops.ErrForbidden)
	grant.Permissions = append(grant.Permissions, agentgrant.PermissionDraftEdit)
	row, err = svc.Get(ctx, grant, row.IntroID)
	require.NoError(t, err)
	assert.Equal(t, "private", row.Body)
	grant.Sources = nil
	_, err = svc.Get(ctx, grant, row.IntroID)
	assert.ErrorIs(t, err, introops.ErrForbidden)
	_, err = svc.Get(ctx, grant, "missing")
	assert.ErrorIs(t, err, introops.ErrForbidden)
}

func TestListFiltersSourcesBeforePagination(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	allowed, err := f.Store.GetOrCreateSource("beeper", "allowed")
	require.NoError(t, err)
	other, err := f.Store.GetOrCreateSource("beeper", "other")
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("list-a@example.com", "A", "example.com"))
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("list-b@example.com", "B", "example.com"))
	require.NoError(t, err)
	for _, id := range []int64{allowed.ID, other.ID} {
		_, err = f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: id, PersonAID: a.ID, PersonBID: b.ID, Body: "hidden"}, "owner")
		require.NoError(t, err)
	}
	g := &agentgrant.Grant{ID: "reader", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: 999, Type: "beeper", Identifier: "allowed"}}}
	svc := introops.Service{Store: f.Store}
	first, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	assert.Equal(t, allowed.ID, first.Items[0].SourceID)
	assert.Empty(t, first.Items[0].Body)
	assert.False(t, first.HasMore)
}

func TestPrepareRejectsNonpositiveSourceID(t *testing.T) {
	f := storetest.New(t)
	svc := introops.Service{Store: f.Store}
	for _, id := range []int64{0, -1} {
		_, err := svc.Prepare(t.Context(), nil, store.PersonIntroductionPrepare{SourceID: id})
		assert.ErrorIs(t, err, store.ErrIntroductionInvalid)
		assert.Equal(t, "invalid_args", introops.ErrorCode(err))
	}
}
