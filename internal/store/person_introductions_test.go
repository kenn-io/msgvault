package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonIntroductionLifecycle(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("a@example.com", "A", "example.com"))
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("b@example.com", "B", "example.com"))
	require.NoError(t, err)
	in := store.PersonIntroductionPrepare{SourceID: source.ID, PersonAID: a.ID, PersonBID: b.ID, Title: "Hello", Body: "A meets B"}
	row, err := f.Store.PreparePersonIntroductionContext(ctx, in, "owner")
	require.NoError(t, err)
	assert.Equal(t, int64(1), row.Revision)
	assert.Equal(t, "prepared", row.Status)
	title := "Updated"
	row, err = f.Store.EditPersonIntroductionContext(ctx, row.IntroID, store.PersonIntroductionEdit{Revision: 1, Title: &title})
	require.NoError(t, err)
	assert.Equal(t, int64(2), row.Revision)
	_, err = f.Store.CancelPersonIntroductionContext(ctx, row.IntroID, 1)
	assert.ErrorIs(t, err, store.ErrIntroductionRevision)
	page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	row, err = f.Store.CancelPersonIntroductionContext(ctx, row.IntroID, 2)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", row.Status)
	assert.Empty(t, row.Body)
	_, err = f.Store.EditPersonIntroductionContext(ctx, row.IntroID, store.PersonIntroductionEdit{Revision: 3, Title: &title})
	assert.ErrorIs(t, err, store.ErrIntroductionState)
	var body *string
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, f.Store.Rebind("SELECT body FROM person_introductions WHERE intro_id = ?"), row.IntroID).Scan(&body))
	assert.Nil(t, body)
	in.PersonBID = in.PersonAID
	_, err = f.Store.PreparePersonIntroductionContext(ctx, in, "owner")
	assert.ErrorIs(t, err, store.ErrIntroductionSamePerson)
}

func TestIntroductionMergeAndSplit(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "merge-account")
	require.NoError(t, err)
	pa := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	pb := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	pc := f.EnsureParticipant("third@example.com", "Third", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	c, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	require.NoError(t, err)
	prepare := func(x, y int64) store.PersonIntroduction {
		v, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: source.ID, PersonAID: x, PersonBID: y, Title: "hello", Body: "body"}, "owner")
		require.NoError(t, err)
		return v
	}
	left := prepare(b.ID, c.ID)
	right := prepare(c.ID, b.ID)
	collapsed := prepare(a.ID, b.ID)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: a.ID, AbsorbedID: b.ID, ExpectedSurvivorRevision: a.Revision, ExpectedAbsorbedRevision: b.Revision, IdempotencyKey: "intro-merge", Actor: "test"})
	require.NoError(t, err)
	got, err := f.Store.GetPersonIntroductionContext(ctx, left.IntroID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.PersonAID)
	assert.Equal(t, int64(2), got.Revision)
	got, err = f.Store.GetPersonIntroductionContext(ctx, right.IntroID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.PersonBID)
	_, err = f.Store.GetPersonIntroductionContext(ctx, collapsed.IntroID)
	assert.ErrorIs(t, err, store.ErrIntroductionNotFound)
	title := "edited after merge"
	_, err = f.Store.EditPersonIntroductionContext(ctx, left.IntroID, store.PersonIntroductionEdit{Revision: 2, Title: &title})
	require.NoError(t, err)
	_, err = f.Store.CancelPersonIntroductionContext(ctx, right.IntroID, 2)
	require.NoError(t, err)
	split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: a.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{pb}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "intro-split", Actor: "test"})
	require.NoError(t, err)
	got, err = f.Store.GetPersonIntroductionContext(ctx, left.IntroID)
	require.NoError(t, err)
	assert.Equal(t, split.NewPerson.ID, got.PersonAID)
	assert.Equal(t, title, got.Title)
	assert.Greater(t, got.Revision, int64(3))
	got, err = f.Store.GetPersonIntroductionContext(ctx, right.IntroID)
	require.NoError(t, err)
	assert.Equal(t, split.NewPerson.ID, got.PersonBID)
	assert.Equal(t, "cancelled", got.Status)
	assert.Empty(t, got.Body)
	assert.Greater(t, got.Revision, int64(3))
	got, err = f.Store.GetPersonIntroductionContext(ctx, collapsed.IntroID)
	require.NoError(t, err)
	assert.Equal(t, split.NewPerson.ID, got.PersonBID)
}

func TestIntroductionPaginationAndScope(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	one, err := f.Store.GetOrCreateSource("beeper", "one")
	require.NoError(t, err)
	two, err := f.Store.GetOrCreateSource("beeper", "two")
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("page-a@example.com", "A", "example.com"))
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("page-b@example.com", "B", "example.com"))
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, source := range []int64{one.ID, one.ID, one.ID, two.ID} {
		row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: source, PersonAID: a.ID, PersonBID: b.ID}, "owner")
		require.NoError(t, err)
		if source == one.ID {
			ids[row.IntroID] = true
		}
	}
	cursor := ""
	seen := map[string]bool{}
	finished := false
	for range 4 {
		page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{Limit: 1, AfterID: cursor}, []int64{one.ID})
		require.NoError(t, err)
		for _, row := range page.Items {
			assert.True(t, ids[row.IntroID])
			assert.False(t, seen[row.IntroID])
			seen[row.IntroID] = true
		}
		if !page.HasMore {
			finished = true
			break
		}
		require.NotEmpty(t, page.NextAfterID)
		cursor = page.NextAfterID
		require.LessOrEqual(t, len(seen), 3)
	}
	assert.True(t, finished)
	assert.Len(t, seen, 3)
	empty, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, []int64{})
	require.NoError(t, err)
	assert.Empty(t, empty.Items)
}

func TestIntroductionNonPreparedStatesAndCascade(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "states")
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("states-a@example.com", "A", "example.com"))
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("states-b@example.com", "B", "example.com"))
	require.NoError(t, err)
	for _, status := range []string{"creating", "group_created", "drafted", "remote_unknown", "cancelled"} {
		row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID, Body: "private"}, "owner")
		require.NoError(t, err)
		_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE person_introductions SET status = ? WHERE intro_id = ?`), status, row.IntroID)
		require.NoError(t, err)
		body := "changed"
		_, err = f.Store.EditPersonIntroductionContext(ctx, row.IntroID, store.PersonIntroductionEdit{Revision: 1, Body: &body})
		assert.ErrorIs(t, err, store.ErrIntroductionState)
		_, err = f.Store.CancelPersonIntroductionContext(ctx, row.IntroID, 1)
		assert.ErrorIs(t, err, store.ErrIntroductionState)
		got, err := f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
		require.NoError(t, err)
		assert.Equal(t, "private", got.Body)
		assert.Equal(t, int64(1), got.Revision)
	}
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM persons WHERE id = ?`), b.ID)
	require.NoError(t, err)
	page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, nil)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
}

func TestIntroductionRefusesRetiredSource(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "retired")
	require.NoError(t, err)
	target, err := f.Store.GetOrCreateSource("beeper", "active")
	require.NoError(t, err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("retired-a@example.com", "A", "example.com"))
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("retired-b@example.com", "B", "example.com"))
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO source_settings (source_id, merged_into_source_id) VALUES (?, ?) ON CONFLICT (source_id) DO UPDATE SET merged_into_source_id = excluded.merged_into_source_id`), src.ID, target.ID)
	require.NoError(t, err)
	_, err = f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID}, "owner")
	assert.ErrorIs(t, err, store.ErrSourceRetired)
	page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, nil)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
}

func TestIntroductionSplitDoesNotRestoreDeletedSource(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "deleted-source")
	require.NoError(t, err)
	pa := f.EnsureParticipant("deleted-a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("deleted-b@example.com", "B", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID}, "owner")
	require.NoError(t, err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: a.ID, AbsorbedID: b.ID, ExpectedSurvivorRevision: a.Revision, ExpectedAbsorbedRevision: b.Revision, IdempotencyKey: "deleted-source-merge", Actor: "test"})
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM sources WHERE id = ?`), src.ID)
	require.NoError(t, err)
	split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: a.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{pb}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "deleted-source-split", Actor: "test"})
	require.NoError(t, err)
	assert.NotEmpty(t, split.UnrestoredRows)
	_, err = f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
	assert.ErrorIs(t, err, store.ErrIntroductionNotFound)
}

func TestIntroductionSplitKeepsDeletedCounterpartAbsent(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "counterpart")
	require.NoError(t, err)
	pa := f.EnsureParticipant("counter-a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("counter-b@example.com", "B", "example.com")
	pc := f.EnsureParticipant("counter-c@example.com", "C", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	c, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	require.NoError(t, err)
	row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: b.ID, PersonBID: c.ID}, "owner")
	require.NoError(t, err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: a.ID, AbsorbedID: b.ID, ExpectedSurvivorRevision: a.Revision, ExpectedAbsorbedRevision: b.Revision, IdempotencyKey: "counter-merge", Actor: "test"})
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM persons WHERE id = ?`), c.ID)
	require.NoError(t, err)
	_, err = f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: a.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{pb}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "counter-split", Actor: "test"})
	require.NoError(t, err)
	_, err = f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
	assert.ErrorIs(t, err, store.ErrIntroductionNotFound)
}

func TestIntroductionMultiStageMerge(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "multi")
	require.NoError(t, err)
	pa := f.EnsureParticipant("multi-a@example.com", "A", "example.com")
	pb := f.EnsureParticipant("multi-b@example.com", "B", "example.com")
	pc := f.EnsureParticipant("multi-c@example.com", "C", "example.com")
	pd := f.EnsureParticipant("multi-d@example.com", "D", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	c, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	require.NoError(t, err)
	d, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pd)
	require.NoError(t, err)
	row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: b.ID, PersonBID: d.ID}, "owner")
	require.NoError(t, err)
	first, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: a.ID, AbsorbedID: b.ID, ExpectedSurvivorRevision: a.Revision, ExpectedAbsorbedRevision: b.Revision, IdempotencyKey: "multi-first", Actor: "test"})
	require.NoError(t, err)
	second, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: c.ID, AbsorbedID: a.ID, ExpectedSurvivorRevision: c.Revision, ExpectedAbsorbedRevision: first.Person.Revision, IdempotencyKey: "multi-second", Actor: "test"})
	require.NoError(t, err)
	got, err := f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
	require.NoError(t, err)
	assert.Equal(t, c.ID, got.PersonAID)
	assert.Equal(t, int64(3), got.Revision)
	split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: c.ID, MergeID: second.Merge.ID, ParticipantIDs: []int64{pa, pb}, ExpectedSourceRevision: second.Person.Revision, IdempotencyKey: "multi-undo-second", Actor: "test"})
	require.NoError(t, err)
	got, err = f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
	require.NoError(t, err)
	assert.Equal(t, split.NewPerson.ID, got.PersonAID)
	assert.Greater(t, got.Revision, int64(3))
}
