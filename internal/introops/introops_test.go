package introops_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
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
	page, err := svc.List(ctx, grant, store.PersonIntroductionQuery{})
	req.NoError(err)
	req.Len(page.Items, 1)
	check.Empty(page.Items[0].Body)
	for _, member := range page.Items[0].Members {
		check.Empty(member.UserID)
	}
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

func TestDelegatedListFiltersBeforeLimit(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "account")
	req.NoError(err)
	other, err := f.Store.GetOrCreateSource("beeper", "other")
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
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("other", "a"), pb, beeperidentity.Fallback("other", "b"))
	req.NoError(err)
	svc := introops.Service{Store: f.Store}
	in := store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID}
	g := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "account"}}}
	empty, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	otherRow, err := svc.Prepare(ctx, nil, store.PersonIntroductionPrepare{SourceID: other.ID, PersonAID: a.ID, PersonBID: b.ID})
	req.NoError(err)
	filtered, err := svc.List(ctx, g, store.PersonIntroductionQuery{SourceID: other.ID})
	req.NoError(err)
	for _, row := range filtered.Items {
		check.NotEqual(otherRow.IntroID, row.IntroID)
	}
	in.PersonBID = c.ID
	for range 101 {
		_, err = svc.Prepare(ctx, nil, in)
		req.NoError(err)
	}
	page, err := svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	assert.Equal(t, empty, page)
	in.PersonBID = b.ID
	visible, err := svc.Prepare(ctx, nil, in)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE person_introductions SET created_at = ? WHERE intro_id = ?`), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), visible.IntroID)
	req.NoError(err)
	page, err = svc.List(ctx, g, store.PersonIntroductionQuery{Limit: 1})
	req.NoError(err)
	req.Len(page.Items, 1)
	check.Equal(visible.IntroID, page.Items[0].IntroID)
	check.False(page.HasMore)
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
	absent, cross, owner := person("absent", ""), person("cross", "scope-b"), person("self", "scope-a")
	scopedOwner := person("scoped-owner", "scope-a")
	var scopedParticipant int64
	req.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), scopedOwner).Scan(&scopedParticipant))
	req.NoError(f.Store.AddAccountIdentityContext(ctx, other.ID, "self", "manual"))
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), scopedParticipant, beeperidentity.Fallback("scope-b", "self"))
	req.NoError(err)
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
	parity := func(id string, wantVisible bool) {
		t.Helper()
		_, err := svc.Get(ctx, g, id)
		if wantVisible {
			req.NoError(err)
		} else {
			req.ErrorIs(err, introops.ErrForbidden)
		}
		page, err := svc.List(ctx, g, store.PersonIntroductionQuery{})
		req.NoError(err)
		found := false
		for _, row := range page.Items {
			if row.IntroID == id {
				found = true
			}
		}
		check.Equal(wantVisible, found)
	}
	parity(visible.IntroID, true)
	var participant int64
	req.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), b).Scan(&participant))
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?)`), participant, beeperidentity.Fallback("scope-a", "second-b"))
	req.NoError(err)
	parity(visible.IntroID, true) // ambiguous identities remain visible
	for _, id := range []int64{absent, cross, owner, scopedOwner, 99999999} {
		in.PersonBID = id
		_, err := svc.Prepare(ctx, g, in)
		req.ErrorIs(err, introops.ErrForbidden)
		check.Equal("not_permitted", introops.ErrorCode(err))
	}
	pre, err := f.Store.GetPersonIntroductionPreflightContext(ctx, src.ID, a, cross)
	req.NoError(err)
	check.Contains(pre.Blockers, store.IntroductionBlocker{Code: "no_identity_on_account", PersonID: cross})
	rows := []introops.Record{visible}
	for _, id := range []int64{absent, cross, owner, scopedOwner} {
		in.PersonBID = id
		row, err := svc.Prepare(ctx, nil, in)
		req.NoError(err)
		parity(row.IntroID, false)
		_, err = svc.Cancel(ctx, g, row.IntroID, row.Revision)
		req.ErrorIs(err, introops.ErrForbidden)
		rows = append(rows, row)
	}
	all, err := svc.List(ctx, nil, store.PersonIntroductionQuery{})
	req.NoError(err)
	check.Len(all.Items, 5)
	pa, err := f.Store.GetPersonContext(ctx, a)
	req.NoError(err)
	pb, err := f.Store.GetPersonContext(ctx, b)
	req.NoError(err)
	_, err = f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: a, AbsorbedID: b, ExpectedSurvivorRevision: pa.Revision, ExpectedAbsorbedRevision: pb.Revision, IdempotencyKey: "visibility-collapse", Actor: "test"})
	req.NoError(err)
	parity(visible.IntroID, true) // collapsed participants remain visible
	// Person merge changes the revision; cancel every prepared row before source retirement.
	for _, row := range rows {
		current, err := svc.Get(ctx, nil, row.IntroID)
		req.NoError(err)
		_, err = svc.Cancel(ctx, nil, row.IntroID, current.Revision)
		req.NoError(err)
	}
	_, err = f.Store.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	req.NoError(err)
	parity(visible.IntroID, true) // retired sources remain visible
}

// catalogPause pauses the real catalog read after its rows have been consumed.
type catalogPause struct {
	ready, resume chan struct{}
	once          sync.Once
}

func (h *catalogPause) Enabled(context.Context, slog.Level) bool { return true }
func (h *catalogPause) Handle(_ context.Context, r slog.Record) error {
	var stmt string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "stmt" {
			stmt = a.Value.String()
		}
		return true
	})
	if (r.Message == "sql" || r.Message == "sql slow") && strings.Contains(stmt, "FROM sources WHERE 1=1") {
		h.once.Do(func() { close(h.ready); <-h.resume })
	}
	return nil
}
func (h *catalogPause) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *catalogPause) WithGroup(string) slog.Handler      { return h }

func TestDelegatedListSnapshotSourceReplacement(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite source IDs are reused after deletion")
	}
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "account-a")
	req.NoError(err)
	pa := f.EnsureParticipant("race-a@example.test", "A", "example.test")
	pb := f.EnsureParticipant("race-b@example.test", "B", "example.test")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	g := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: "account-a"}}}
	h := &catalogPause{ready: make(chan struct{}), resume: make(chan struct{})}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	var resume sync.Once
	defer resume.Do(func() { close(h.resume) })
	type result struct {
		page introops.Page
		err  error
	}
	done := make(chan result, 1)
	go func() {
		page, err := (introops.Service{Store: f.Store}).List(ctx, g, store.PersonIntroductionQuery{})
		done <- result{page, err}
	}()
	<-h.ready
	_, _, err = f.Store.RemoveSourceSerialized(ctx, src.ID)
	req.NoError(err)
	replacement, err := f.Store.GetOrCreateSource("beeper", "account-b")
	req.NoError(err)
	req.Equal(src.ID, replacement.ID)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("account-b", "a"), pb, beeperidentity.Fallback("account-b", "b"))
	req.NoError(err)
	_, err = f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: replacement.ID, PersonAID: a.ID, PersonBID: b.ID, Title: "outside"}, "owner")
	req.NoError(err)
	resume.Do(func() { close(h.resume) })
	got := <-done
	req.NoError(got.err)
	check.Empty(got.page.Items)
	check.False(got.page.HasMore)
}

func TestDelegatedListSameReferenceReimport(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "reimport")
	req.NoError(err)
	g := &agentgrant.Grant{ID: "agent", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: src.ID, Type: "beeper", Identifier: "reimport"}}}
	_, _, err = f.Store.RemoveSourceSerialized(ctx, src.ID)
	req.NoError(err)
	_, err = f.Store.GetOrCreateSource("beeper", "intervening")
	req.NoError(err)
	reimported, err := f.Store.GetOrCreateSource("beeper", "reimport")
	req.NoError(err)
	req.NotEqual(src.ID, reimported.ID)
	pa := f.EnsureParticipant("reimport-a@example.test", "A", "example.test")
	pb := f.EnsureParticipant("reimport-b@example.test", "B", "example.test")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO participant_identifiers (participant_id, identifier_type, identifier_value) VALUES (?, 'beeper', ?), (?, 'beeper', ?)`), pa, beeperidentity.Fallback("reimport", "a"), pb, beeperidentity.Fallback("reimport", "b"))
	req.NoError(err)
	svc := introops.Service{Store: f.Store}
	row, err := svc.Prepare(ctx, nil, store.PersonIntroductionPrepare{SourceID: reimported.ID, PersonAID: a.ID, PersonBID: b.ID, Body: "private"})
	req.NoError(err)
	page, err := svc.List(ctx, g, store.PersonIntroductionQuery{})
	req.NoError(err)
	req.Len(page.Items, 1)
	check.Equal(row.IntroID, page.Items[0].IntroID)
}
