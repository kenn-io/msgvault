package introops_test

import (
	"testing"
	"time"

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
	req.Len(row.Members, 2)
	for _, member := range row.Members {
		check.Empty(member.UserID)
	}
	title := "changed"
	_, err = svc.Edit(ctx, grant, row.IntroID, store.PersonIntroductionEdit{Revision: 1, Title: &title})
	req.ErrorIs(err, introops.ErrForbidden)
	grant.Permissions = append(grant.Permissions, agentgrant.PermissionDraftEdit)
	_, err = svc.Edit(ctx, grant, row.IntroID, store.PersonIntroductionEdit{Revision: 1, Title: &title})
	req.ErrorIs(err, introops.ErrForbidden)
	row, err = svc.Get(ctx, grant, row.IntroID)
	req.NoError(err)
	check.Equal("private", row.Body)
	for _, member := range row.Members {
		check.Empty(member.UserID)
	}
	ownerRow, err := svc.Get(ctx, nil, row.IntroID)
	req.NoError(err)
	check.NotEmpty(ownerRow.Members[0].UserID)
	other, err := f.Store.GetOrCreateSource("beeper", "other-account")
	req.NoError(err)
	grant.Sources = []agentgrant.SourceRef{{ID: other.ID, Type: "beeper", Identifier: "other-account"}}
	_, err = svc.Get(ctx, grant, row.IntroID)
	req.ErrorIs(err, introops.ErrForbidden)
	_, err = svc.Get(ctx, grant, "missing")
	req.ErrorIs(err, introops.ErrForbidden)
}

func TestDelegatedListContinuesPastScanLimit(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "account")
	req.NoError(err)
	pa := f.EnsureParticipant("scan-a@example.test", "A", "example.test")
	pb := f.EnsureParticipant("scan-b@example.test", "B", "example.test")
	pc := f.EnsureParticipant("scan-hidden@example.test", "C", "example.test")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	c, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("account", "a"), pb, beeperidentity.Fallback("account", "b"))
	req.NoError(err)
	svc := introops.Service{Store: f.Store}
	in := store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID}
	visible, err := svc.Prepare(ctx, nil, in)
	req.NoError(err)
	in.PersonBID = c.ID
	for range 101 {
		_, err = svc.Prepare(ctx, nil, in)
		req.NoError(err)
	}
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE person_introductions SET created_at = ? WHERE intro_id = ?`), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), visible.IntroID)
	req.NoError(err)
	g := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "account"}}}
	page, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	check.Empty(page.Items)
	check.True(page.HasMore)
	req.NotEmpty(page.NextAfterID)
	page, err = svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1, AfterID: page.NextAfterID})
	req.NoError(err)
	req.Len(page.Items, 1)
	check.Equal(visible.IntroID, page.Items[0].IntroID)
	check.False(page.HasMore)
	// Resume from a visible boundary without skipping another visible record.
	in.PersonBID = b.ID
	newer, err := svc.Prepare(ctx, nil, in)
	req.NoError(err)
	second, err := svc.Prepare(ctx, nil, in)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE person_introductions SET created_at = ? WHERE intro_id = ?`), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), newer.IntroID)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE person_introductions SET created_at = ? WHERE intro_id = ?`), time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), second.IntroID)
	req.NoError(err)
	page, err = svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	req.Len(page.Items, 1)
	check.Equal(newer.IntroID, page.Items[0].IntroID)
	check.True(page.HasMore)
	page, err = svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1, AfterID: page.NextAfterID})
	req.NoError(err)
	req.Len(page.Items, 1)
	check.Equal(second.IntroID, page.Items[0].IntroID)
	check.True(page.HasMore)
	page, err = svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1, AfterID: page.NextAfterID})
	req.NoError(err)
	req.Len(page.Items, 1)
	check.Equal(visible.IntroID, page.Items[0].IntroID)
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
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("other", "a"), pb, beeperidentity.Fallback("other", "b"))
	req.NoError(err)
	for _, id := range []int64{allowed.ID, other.ID} {
		_, err = f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: id, PersonAID: a.ID, PersonBID: b.ID, Body: "hidden"}, "owner")
		req.NoError(err)
	}
	g := &agentgrant.Grant{ID: "reader", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: allowed.ID, Type: "beeper", Identifier: "allowed"}}}
	svc := introops.Service{Store: f.Store}
	first, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	req.Len(first.Items, 1)
	check.Equal(allowed.ID, first.Items[0].SourceID)
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
		t.Helper()
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
	absent, owner := person("absent", ""), person("self", "scope-a")
	req.NoError(f.Store.AddAccountIdentityContext(ctx, other.ID, "self@example.test", "manual"))
	svc := introops.Service{Store: f.Store}
	g := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: src.ID, Type: "beeper", Identifier: "scope-a"}}}
	in := store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a, PersonBID: b, Body: "private"}
	unsupported, err := f.Store.GetOrCreateSource("gmail", "other@example.test")
	req.NoError(err)
	otherGrant := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "gmail", Identifier: "other@example.test"}}}
	badSource := in
	badSource.SourceID = unsupported.ID
	_, err = svc.Prepare(ctx, otherGrant, badSource)
	req.ErrorIs(err, introops.ErrForbidden)
	badSource.SourceID = 99999999
	_, err = svc.Prepare(ctx, otherGrant, badSource)
	req.ErrorIs(err, introops.ErrForbidden)
	visible, err := svc.Prepare(ctx, g, in)
	req.NoError(err)
	check.Empty(visible.Body)
	// A second account identity is a visible blocker, not a permission denial.
	var participant int64
	req.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), b).Scan(&participant))
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), participant, beeperidentity.Fallback("scope-a", "second-b"))
	req.NoError(err)
	ambiguous, err := svc.Get(ctx, g, visible.IntroID)
	req.NoError(err)
	check.Contains(ambiguous.Blockers, store.IntroductionBlocker{Code: "ambiguous_identity", PersonID: b})
	for _, id := range []int64{absent, owner, 99999999} {
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
	for _, id := range []int64{owner} {
		in.PersonBID = id
		row, err := svc.Prepare(ctx, nil, in)
		req.NoError(err)
		_, err = svc.Get(ctx, g, row.IntroID)
		req.ErrorIs(err, introops.ErrForbidden)
		_, err = svc.Cancel(ctx, g, row.IntroID, row.Revision)
		req.ErrorIs(err, introops.ErrForbidden)
	}
	all, err := svc.List(ctx, nil, store.PersonIntroductionQuery{})
	req.NoError(err)
	check.Len(all.Items, 3)
}
