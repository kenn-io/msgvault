package store_test

import (
	"encoding/base64"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonIntroductionLifecycle(t *testing.T) { checkPersonIntroductionLifecycle(t) }
func TestPostgresPersonIntroductionLifecycle(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL test database not configured")
	}
	checkPersonIntroductionLifecycle(t)
}
func checkPersonIntroductionLifecycle(t *testing.T) {
	t.Helper()
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "synthetic-account")
	req.NoError(err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("a@example.com", "A", "example.com"))
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("b@example.com", "B", "example.com"))
	req.NoError(err)
	in := store.PersonIntroductionPrepare{SourceID: source.ID, PersonAID: a.ID, PersonBID: b.ID, Title: "Hello", Body: "A meets B"}
	row, err := f.Store.PreparePersonIntroductionContext(ctx, in, "owner")
	req.NoError(err)
	check.Equal(int64(1), row.Revision)
	check.Equal("prepared", row.Status)
	title := "Updated"
	row, err = f.Store.EditPersonIntroductionContext(ctx, row.IntroID, store.PersonIntroductionEdit{Revision: 1, Title: &title})
	req.NoError(err)
	check.Equal(int64(2), row.Revision)
	_, err = f.Store.CancelPersonIntroductionContext(ctx, row.IntroID, 1)
	req.ErrorIs(err, store.ErrIntroductionRevision)
	page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, nil)
	req.NoError(err)
	req.Len(page.Items, 1)
	row, err = f.Store.CancelPersonIntroductionContext(ctx, row.IntroID, 2)
	req.NoError(err)
	check.Equal("cancelled", row.Status)
	check.Empty(row.Body)
	_, err = f.Store.EditPersonIntroductionContext(ctx, row.IntroID, store.PersonIntroductionEdit{Revision: 3, Title: &title})
	req.ErrorIs(err, store.ErrIntroductionState)
	_, err = f.Store.CancelPersonIntroductionContext(ctx, row.IntroID, row.Revision)
	req.ErrorIs(err, store.ErrIntroductionState)
	got, err := f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
	req.NoError(err)
	check.Equal(row.Revision, got.Revision)
	var body *string
	req.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind("SELECT body FROM person_introductions WHERE intro_id = ?"), row.IntroID).Scan(&body))
	check.Nil(body)
	in.PersonBID = in.PersonAID
	_, err = f.Store.PreparePersonIntroductionContext(ctx, in, "owner")
	req.ErrorIs(err, store.ErrIntroductionSamePerson)
}

func TestIntroductionMergeAndSplit(t *testing.T) { checkIntroductionMergeAndSplit(t) }
func TestPostgresIntroductionMergeAndSplit(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL test database not configured")
	}
	checkIntroductionMergeAndSplit(t)
}
func checkIntroductionMergeAndSplit(t *testing.T) {
	t.Helper()
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource("beeper", "merge-account")
	req.NoError(err)
	pa := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	pb := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	pc := f.EnsureParticipant("third@example.com", "Third", "example.com")
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	c, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
	req.NoError(err)
	prepare := func(x, y int64) store.PersonIntroduction {
		t.Helper()
		v, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: source.ID, PersonAID: x, PersonBID: y, Title: "hello", Body: "body"}, "owner")
		req.NoError(err)
		return v
	}
	left := prepare(b.ID, c.ID)
	right := prepare(c.ID, b.ID)
	collapsed := prepare(a.ID, b.ID)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: a.ID, AbsorbedID: b.ID, ExpectedSurvivorRevision: a.Revision, ExpectedAbsorbedRevision: b.Revision, IdempotencyKey: "intro-merge", Actor: "test"})
	req.NoError(err)
	got, err := f.Store.GetPersonIntroductionContext(ctx, left.IntroID)
	req.NoError(err)
	check.Equal(a.ID, got.PersonAID)
	check.Equal(int64(2), got.Revision)
	got, err = f.Store.GetPersonIntroductionContext(ctx, right.IntroID)
	req.NoError(err)
	check.Equal(a.ID, got.PersonBID)
	got, err = f.Store.GetPersonIntroductionContext(ctx, collapsed.IntroID)
	req.NoError(err)
	check.Equal(a.ID, got.PersonAID)
	check.Equal(a.ID, got.PersonBID)
	check.Equal(int64(2), got.Revision)
	preflight, err := f.Store.GetPersonIntroductionPreflightContext(ctx, source.ID, got.PersonAID, got.PersonBID)
	req.NoError(err)
	check.Contains(preflight.Blockers, store.IntroductionBlocker{Code: "same_person", PersonID: a.ID})
	body := "edited body"
	_, err = f.Store.EditPersonIntroductionContext(ctx, collapsed.IntroID, store.PersonIntroductionEdit{Revision: 2, Body: &body})
	req.NoError(err)
	title := "edited after merge"
	_, err = f.Store.EditPersonIntroductionContext(ctx, left.IntroID, store.PersonIntroductionEdit{Revision: 2, Title: &title})
	req.NoError(err)
	_, err = f.Store.CancelPersonIntroductionContext(ctx, right.IntroID, 2)
	req.NoError(err)
	split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: a.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{pb}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "intro-split", Actor: "test"})
	req.NoError(err)
	got, err = f.Store.GetPersonIntroductionContext(ctx, left.IntroID)
	req.NoError(err)
	check.Equal(split.NewPerson.ID, got.PersonAID)
	check.Equal(title, got.Title)
	check.Greater(got.Revision, int64(3))
	got, err = f.Store.GetPersonIntroductionContext(ctx, right.IntroID)
	req.NoError(err)
	check.Equal(split.NewPerson.ID, got.PersonBID)
	check.Equal("cancelled", got.Status)
	check.Empty(got.Body)
	check.Greater(got.Revision, int64(3))
	got, err = f.Store.GetPersonIntroductionContext(ctx, collapsed.IntroID)
	req.NoError(err)
	check.Equal(split.NewPerson.ID, got.PersonBID)
	check.Equal(body, got.Body)
	check.Greater(got.Revision, int64(3))
	t.Run("multi-stage merge", func(t *testing.T) {
		check := assert.New(t)
		req := require.New(t)
		f := storetest.New(t)
		ctx := t.Context()
		src, err := f.Store.GetOrCreateSource("beeper", "multi")
		req.NoError(err)
		pa := f.EnsureParticipant("multi-a@example.com", "A", "example.com")
		pb := f.EnsureParticipant("multi-b@example.com", "B", "example.com")
		pc := f.EnsureParticipant("multi-c@example.com", "C", "example.com")
		pd := f.EnsureParticipant("multi-d@example.com", "D", "example.com")
		a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pa)
		req.NoError(err)
		b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pb)
		req.NoError(err)
		c, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pc)
		req.NoError(err)
		d, _, err := f.Store.CreatePersonFromParticipantContext(ctx, pd)
		req.NoError(err)
		row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: b.ID, PersonBID: d.ID}, "owner")
		req.NoError(err)
		first, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: a.ID, AbsorbedID: b.ID, ExpectedSurvivorRevision: a.Revision, ExpectedAbsorbedRevision: b.Revision, IdempotencyKey: "multi-first", Actor: "test"})
		req.NoError(err)
		second, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: c.ID, AbsorbedID: a.ID, ExpectedSurvivorRevision: c.Revision, ExpectedAbsorbedRevision: first.Person.Revision, IdempotencyKey: "multi-second", Actor: "test"})
		req.NoError(err)
		got, err := f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
		req.NoError(err)
		check.Equal(c.ID, got.PersonAID)
		check.Equal(int64(3), got.Revision)
		split, err := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: c.ID, MergeID: second.Merge.ID, ParticipantIDs: []int64{pa, pb}, ExpectedSourceRevision: second.Person.Revision, IdempotencyKey: "multi-undo-second", Actor: "test"})
		req.NoError(err)
		got, err = f.Store.GetPersonIntroductionContext(ctx, row.IntroID)
		req.NoError(err)
		check.Equal(split.NewPerson.ID, got.PersonAID)
		check.Greater(got.Revision, int64(3))
	})
}

func TestIntroductionPaginationAndScope(t *testing.T) { checkIntroductionPaginationAndScope(t) }
func TestPostgresIntroductionPaginationAndScope(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL test database not configured")
	}
	checkIntroductionPaginationAndScope(t)
}
func checkIntroductionPaginationAndScope(t *testing.T) {
	t.Helper()
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	one, err := f.Store.GetOrCreateSource("beeper", "one")
	req.NoError(err)
	two, err := f.Store.GetOrCreateSource("beeper", "two")
	req.NoError(err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("page-a@example.com", "A", "example.com"))
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("page-b@example.com", "B", "example.com"))
	req.NoError(err)
	ids := map[string]bool{}
	var ordered []string
	for _, source := range []int64{one.ID, one.ID, one.ID, two.ID} {
		row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: source, PersonAID: a.ID, PersonBID: b.ID}, "owner")
		req.NoError(err)
		if source == one.ID {
			ids[row.IntroID] = true
			ordered = append(ordered, row.IntroID)
			micros := 1
			if len(ordered) == 3 {
				micros = 2
			}
			stamp := time.Date(2026, 1, 1, 0, 0, 0, micros*1000, time.UTC)
			_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE person_introductions SET created_at = ? WHERE intro_id = ?`), stamp, row.IntroID)
			req.NoError(err)
		}
	}
	sort.Strings(ordered[:2])
	cursor := ""
	seen := map[string]bool{}
	finished := false
	for range 4 {
		page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{Limit: 1, AfterID: cursor}, []int64{one.ID})
		req.NoError(err)
		for _, row := range page.Items {
			check.True(ids[row.IntroID])
			check.False(seen[row.IntroID])
			seen[row.IntroID] = true
			req.LessOrEqual(len(seen), len(ordered))
			check.Equal(ordered[len(ordered)-len(seen)], row.IntroID)
		}
		if !page.HasMore {
			finished = true
			break
		}
		req.NotEmpty(page.NextAfterID)
		cursor = page.NextAfterID
		req.LessOrEqual(len(seen), 3)
	}
	check.True(finished)
	check.Len(seen, 3)
	empty, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, []int64{})
	req.NoError(err)
	check.Empty(empty.Items)
	for _, raw := range []string{
		`{"version":2,"t":"2026-01-01T00:00:00Z","i":"x"}`,
		`{"version":1,"t":"2026-01-01T00:00:00Z","i":"x","extra":true}`,
		`{"version":1,"t":"2026-01-01T00:00:00Z","i":"x"} {}`,
	} {
		_, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{AfterID: base64.RawURLEncoding.EncodeToString([]byte(raw))}, nil)
		req.ErrorIs(err, store.ErrIntroductionInvalid)
	}
	_, err = f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{AfterID: strings.Repeat("a", 4000)}, nil)
	req.ErrorIs(err, store.ErrIntroductionInvalid)
}

func TestIntroductionPersonDeleteCascades(t *testing.T) { checkIntroductionPersonDeleteCascades(t) }
func TestPostgresIntroductionPersonDeleteCascades(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL test database not configured")
	}
	checkIntroductionPersonDeleteCascades(t)
}
func checkIntroductionPersonDeleteCascades(t *testing.T) {
	t.Helper()
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "cascade")
	req.NoError(err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("cascade-a@example.com", "A", "example.com"))
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("cascade-b@example.com", "B", "example.com"))
	req.NoError(err)
	_, err = f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID}, "owner")
	req.NoError(err)
	err = f.Store.DeletePersonContext(ctx, b.ID, b.Revision)
	req.NoError(err)
	page, err := f.Store.ListPersonIntroductionsContext(ctx, store.PersonIntroductionQuery{}, nil)
	req.NoError(err)
	check.Empty(page.Items)
}

func TestIntroductionRefusesRetiredSource(t *testing.T) { checkIntroductionRefusesRetiredSource(t) }
func TestPostgresIntroductionRefusesRetiredSource(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL test database not configured")
	}
	checkIntroductionRefusesRetiredSource(t)
}
func checkIntroductionRefusesRetiredSource(t *testing.T) {
	t.Helper()
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	src, err := f.Store.GetOrCreateSource("beeper", "retired")
	req.NoError(err)
	target, err := f.Store.GetOrCreateSource("beeper", "active")
	req.NoError(err)
	a, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("retired-a@example.com", "A", "example.com"))
	req.NoError(err)
	b, _, err := f.Store.CreatePersonFromParticipantContext(ctx, f.EnsureParticipant("retired-b@example.com", "B", "example.com"))
	req.NoError(err)
	row, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID, Body: "private"}, "owner")
	req.NoError(err)
	_, err = f.Store.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: target.ID})
	req.ErrorIs(err, store.ErrSourceMergeInvalid)
	_, err = f.Store.CancelPersonIntroductionContext(ctx, row.IntroID, row.Revision)
	req.NoError(err)
	_, err = f.Store.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: target.ID})
	req.NoError(err)
	_, err = f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID}, "owner")
	req.ErrorIs(err, store.ErrSourceRetired)
	preflight, err := f.Store.GetPersonIntroductionPreflightContext(ctx, src.ID, a.ID, b.ID)
	req.NoError(err)
	check.Contains(preflight.Blockers, store.IntroductionBlocker{Code: "source_retired"})
}
