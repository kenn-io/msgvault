package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConcurrentAndHistoricalEmploymentsStayIndependentlyQueryable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	dayJob := mustOrganization(t, st, "Example Org")
	sideJob := mustOrganization(t, st, "Another Org")
	pastJob := mustOrganization(t, st, "Former Org")

	past, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: pastJob.ID, Title: new("Junior Engineer"), StartDate: mustPartialDate(t, "2015"), EndDate: mustPartialDate(t, "2018-06"), IsCurrent: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	assert.False(past.IsCurrent)
	assert.False(past.IsPrimary)
	primary, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: dayJob.ID, Title: new("Staff Engineer"), Role: new("Engineering"), Department: new("Archive Platform"), StartDate: mustPartialDate(t, "2018-07"), Source: store.ProvenanceUser})
	require.NoError(err)
	assert.True(primary.IsCurrent)
	assert.True(primary.IsPrimary)
	side, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: sideJob.ID, Title: new("Advisor"), StartDate: mustPartialDate(t, "2022-01"), Source: store.ProvenanceUser})
	require.NoError(err)
	assert.True(side.IsCurrent)
	assert.False(side.IsPrimary)
	all, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(all, 3)
	assert.Equal([]int64{primary.ID, side.ID, past.ID}, employmentIDs(all))
	currentOnly, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: person.ID, CurrentOnly: true})
	require.NoError(err)
	assert.Equal([]int64{primary.ID, side.ID}, employmentIDs(currentOnly))
	byOrganization, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{OrganizationID: pastJob.ID})
	require.NoError(err)
	require.Len(byOrganization, 1)
	assert.Equal(past.ID, byOrganization[0].ID)
	require.NotNil(byOrganization[0].EndDate)
	assert.Equal("2018-06", byOrganization[0].EndDate.String())
}

func TestAtMostOnePrimaryCurrentEmploymentIsEnforced(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "bob@example.com", "bob")
	first := mustOrganization(t, st, "Example Org")
	second := mustOrganization(t, st, "Another Org")
	primary, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: first.ID, Title: new("Engineer"), Source: store.ProvenanceUser, IsPrimary: new(true)})
	require.NoError(err)
	assert.True(primary.IsPrimary)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: second.ID, Title: new("Advisor"), Source: store.ProvenanceUser, IsPrimary: new(true)})
	require.Error(err)
	require.ErrorIs(err, store.ErrEmploymentPrimaryConflict)
	side, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: second.ID, Title: new("Advisor"), Source: store.ProvenanceUser})
	require.NoError(err)
	rotated, err := st.SetPrimaryEmploymentContext(ctx, side.ID, side.Revision)
	require.NoError(err)
	assert.True(rotated.IsPrimary)
	demoted, err := st.GetEmploymentContext(ctx, primary.ID)
	require.NoError(err)
	assert.False(demoted.IsPrimary)
	assert.True(demoted.IsCurrent)
	assert.Equal(primary.Revision+1, demoted.Revision)
}

func TestConcurrentPrimaryRotationDoesNotLeakDatabaseConflicts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for this race regression")
	}
	person := mustPromotedPerson(t, st, "primary-race@example.com", "primary-race")
	firstOrg := mustOrganization(t, st, "Primary Race A")
	secondOrg := mustOrganization(t, st, "Primary Race B")
	first, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: firstOrg.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
		IsPrimary: new(false),
	})
	require.NoError(err)
	second, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: secondOrg.ID,
		Title: new("Advisor"), Source: store.ProvenanceUser,
		IsPrimary: new(false),
	})
	require.NoError(err)

	firstLock, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = firstLock.Rollback() })
	var lockedID int64
	err = firstLock.QueryRowContext(ctx,
		`SELECT id FROM employments WHERE id = $1 FOR UPDATE`, first.ID).Scan(&lockedID)
	require.NoError(err)
	assert.Equal(first.ID, lockedID)

	secondLock, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = secondLock.Rollback() })
	err = secondLock.QueryRowContext(ctx,
		`SELECT id FROM employments WHERE id = $1 FOR UPDATE`, second.ID).Scan(&lockedID)
	require.NoError(err)
	assert.Equal(second.ID, lockedID)

	results := make(chan error, 2)
	go func() {
		_, promoteErr := st.SetPrimaryEmploymentContext(ctx, first.ID, first.Revision)
		results <- promoteErr
	}()
	go func() {
		_, promoteErr := st.SetPrimaryEmploymentContext(ctx, second.ID, second.Revision)
		results <- promoteErr
	}()

	select {
	case early := <-results:
		require.FailNow("promotion bypassed held target row", "error: %v", early)
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(firstLock.Commit())
	require.NoError(secondLock.Commit())

	successes := 0
	for range 2 {
		promoteErr := <-results
		switch {
		case promoteErr == nil:
			successes++
		case errors.Is(promoteErr, store.ErrEmploymentPrimaryConflict),
			errors.Is(promoteErr, store.ErrEmploymentRevisionConflict):
		default:
			require.NoError(promoteErr, "database conflicts must retain a typed classification")
		}
	}
	assert.Positive(successes)

	employments, err := st.ListEmploymentsContext(
		ctx, store.EmploymentFilter{PersonID: person.ID, CurrentOnly: true})
	require.NoError(err)
	primaries := 0
	for _, employment := range employments {
		if employment.IsPrimary {
			primaries++
		}
	}
	assert.Equal(1, primaries)
}

func TestEndEmploymentClearsCurrentAndPrimaryWithoutDeletingHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")
	replacement := mustOrganization(t, st, "Another Org")
	employment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), StartDate: mustPartialDate(t, "2019-04"), Source: store.ProvenanceUser})
	require.NoError(err)
	require.True(employment.IsPrimary)
	endDate, err := store.ParsePartialDate("2024-09")
	require.NoError(err)
	ended, err := st.EndEmploymentContext(ctx, employment.ID, employment.Revision, endDate)
	require.NoError(err)
	assert.False(ended.IsCurrent)
	assert.False(ended.IsPrimary)
	require.NotNil(ended.EndDate)
	assert.Equal("2024-09", ended.EndDate.String())
	assert.Equal(employment.Revision+1, ended.Revision)
	history, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(history, 1)
	next, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: replacement.ID, Title: new("Principal Engineer"), StartDate: mustPartialDate(t, "2024-10"), Source: store.ProvenanceUser, IsPrimary: new(true)})
	require.NoError(err)
	assert.True(next.IsPrimary)
}

func TestEmploymentValidationRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")
	tests := []struct {
		name    string
		input   store.EmploymentInput
		wantErr error
		message string
	}{
		{"missing person", store.EmploymentInput{PersonID: person.ID + 9999, OrganizationID: organization.ID, Source: store.ProvenanceUser}, store.ErrPersonNotFound, ""},
		{"missing organization", store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID + 9999, Source: store.ProvenanceUser}, store.ErrOrganizationNotFound, ""},
		{"end before start", store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, StartDate: mustPartialDate(t, "2020-06"), EndDate: mustPartialDate(t, "2020-05"), Source: store.ProvenanceUser}, store.ErrEmploymentInvalid, "end date must not precede start date"},
		{"year-less start date", store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, StartDate: mustPartialDate(t, "--04-12"), Source: store.ProvenanceUser}, store.ErrEmploymentInvalid, "start date requires a year"},
		{"unknown source", store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Source: "guessed"}, store.ErrInvalidProvenance, ""},
		{"out-of-range confidence", store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Source: store.ProvenanceExtraction, Confidence: new(1.5)}, store.ErrEmploymentInvalid, "confidence must be between 0 and 1"},
		{"confidence on user-declared data", store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Source: store.ProvenanceUser, Confidence: new(0.9)}, store.ErrEmploymentInvalid, "confidence is only meaningful for derived or suggested values"},
		{"confidence on carddav-imported data", store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Source: store.ProvenanceCardDAVImport, Confidence: new(0.9)}, store.ErrEmploymentInvalid, "confidence is only meaningful for derived or suggested values"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, err := st.AddEmploymentContext(ctx, test.input)
			require.Error(err)
			require.ErrorIs(err, test.wantErr)
			if test.message != "" {
				assert.ErrorContains(err, test.message)
			}
		})
	}
}

func TestConfidenceIsAcceptedOnEveryDerivedProvenance(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	derived := []store.Provenance{store.ProvenanceArchiveObservation, store.ProvenanceExtraction, store.ProvenanceEnrichment, store.ProvenanceSystem}
	for i, source := range derived {
		t.Run(string(source), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			assert.False(source.IsDeclared())
			organization := mustOrganization(t, st, fmt.Sprintf("Example Org %d", i))
			employment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), Source: source, Confidence: new(0.75), IsPrimary: new(false)})
			require.NoError(err)
			require.NotNil(employment.Confidence)
			assert.InDelta(0.75, *employment.Confidence, 1e-9)
		})
	}
}

func TestEmploymentColumnChecksAreEnforcedBySQL(t *testing.T) {
	req := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")
	insert := func(t *testing.T, title, columns string, args ...any) error {
		t.Helper()
		query := "INSERT INTO employments (person_id, organization_id, title, title_normalized, source"
		if columns != "" {
			query += ", " + columns
		}
		query += ") VALUES (?, ?, ?, ?, ?"
		var querySb171 strings.Builder
		for range args {
			querySb171.WriteString(", ?")
		}
		query += querySb171.String()
		query += ")"
		full := append([]any{person.ID, organization.ID, title, store.NormalizeEmploymentTitle(&title), string(store.ProvenanceUser)}, args...)
		_, err := st.DB().ExecContext(ctx, st.Rebind(query), full...)
		return err
	}
	rejected := []struct {
		name, columns string
		args          []any
	}{
		{"year below range", "start_year", []any{0}}, {"year above range", "start_year", []any{10000}}, {"month below range", "start_year, start_month", []any{2019, 0}}, {"month above range", "start_year, start_month", []any{2019, 13}}, {"day below range", "start_year, start_month, start_day", []any{2019, 4, 0}}, {"day above range", "start_year, start_month, start_day", []any{2019, 4, 32}}, {"day without month", "start_year, start_day", []any{2019, 12}}, {"month without year", "start_month", []any{4}}, {"end year below range", "end_year", []any{0}}, {"end day without month", "end_year, end_day", []any{2019, 12}},
	}
	for i, test := range rejected {
		t.Run("rejects "+test.name, func(t *testing.T) {
			require := require.New(t)
			require.Error(insert(t, fmt.Sprintf("Rejected %d", i), test.columns, test.args...))
		})
	}
	accepted := []struct {
		name, columns string
		args          []any
	}{
		{"no date at all", "", nil}, {"year only", "start_year", []any{2019}}, {"year and month", "start_year, start_month", []any{2019, 4}}, {"full date", "start_year, start_month, start_day", []any{2019, 4, 12}}, {"boundary values", "start_year, start_month, start_day", []any{1, 12, 31}}, {"end year only", "end_year", []any{2024}}, {"full end date", "end_year, end_month, end_day", []any{2024, 9, 30}}, {"both bounds", "start_year, start_month, end_year, end_month", []any{2019, 4, 2024, 9}},
	}
	for i, test := range accepted {
		t.Run("accepts "+test.name, func(t *testing.T) {
			require := require.New(t)
			require.NoError(insert(t, fmt.Sprintf("Accepted %d", i), test.columns, test.args...))
		})
	}
	t.Run("rejects an unknown provenance", func(t *testing.T) {
		title := "Unknown Source"
		_, err := st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO employments (person_id, organization_id, title, title_normalized, source) VALUES (?, ?, ?, ?, ?)`), person.ID, organization.ID, title, store.NormalizeEmploymentTitle(&title), "guessed")
		req.Error(err)
	})
	t.Run("rejects a confidence on declared data", func(t *testing.T) {
		title := "Declared Confidence"
		_, err := st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO employments (person_id, organization_id, title, title_normalized, source, confidence) VALUES (?, ?, ?, ?, ?, ?)`), person.ID, organization.ID, title, store.NormalizeEmploymentTitle(&title), string(store.ProvenanceUser), 0.9)
		req.Error(err)
	})
	t.Run("accepts a confidence on derived data", func(t *testing.T) {
		title := "Derived Confidence"
		_, err := st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO employments (person_id, organization_id, title, title_normalized, source, confidence) VALUES (?, ?, ?, ?, ?, ?)`), person.ID, organization.ID, title, store.NormalizeEmploymentTitle(&title), string(store.ProvenanceSystem), 0.9)
		req.NoError(err)
	})
}

func TestDuplicateCurrentEmploymentAtTheSameOrganizationAndTitleIsRejected(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")
	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), Source: store.ProvenanceUser})
	require.NoError(err)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("  engineer  "), Source: store.ProvenanceUser})
	require.Error(err)
	require.ErrorIs(err, store.ErrEmploymentDuplicateActive)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Architect"), Source: store.ProvenanceUser})
	require.NoError(err)
}

func TestOrganizationDeletionCannotEraseEmploymentHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")
	employment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), Source: store.ProvenanceUser})
	require.NoError(err)
	err = st.DeleteOrganizationContext(ctx, organization.ID, organization.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationHasEmployments)
	stillThere, err := st.GetEmploymentContext(ctx, employment.ID)
	require.NoError(err)
	assert.Equal(employment.ID, stillThere.ID)
	retired, err := st.ReplaceOrganizationContext(ctx, organization.ID, organization.Revision,
		store.OrganizationInput{Name: "Example Org", Kind: organization.Kind}, true)
	require.NoError(err)
	require.NotNil(retired.RetiredAt)
	survived, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(survived, 1)
	assert.Equal(employment.ID, survived[0].ID)
}

func TestEmploymentCannotTargetAMergedOrganizationRedirect(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	survivor := mustOrganization(t, st, "Example Org")
	losing := mustOrganization(t, st, "Former Org")

	_, err := st.MergeOrganizationsContext(ctx,
		survivor.ID, survivor.Revision, losing.ID, losing.Revision)
	require.NoError(err)

	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: losing.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	require.ErrorContains(err, "merged organization")
}

func TestEmploymentWaitingBehindMergeCannotTargetRedirect(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for this race regression")
	}
	person := mustPromotedPerson(t, st, "merge-race@example.com", "merge-race")
	survivor := mustOrganization(t, st, "Merge Race Survivor")
	losing := mustOrganization(t, st, "Merge Race Losing")

	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var lockedID int64
	err = blocker.QueryRowContext(ctx,
		`SELECT id FROM organizations WHERE id = $1 FOR UPDATE`,
		losing.ID).Scan(&lockedID)
	require.NoError(err)
	require.Equal(losing.ID, lockedID)

	mergeDone := make(chan error, 1)
	go func() {
		_, mergeErr := st.MergeOrganizationsContext(
			ctx, survivor.ID, survivor.Revision, losing.ID, losing.Revision)
		mergeDone <- mergeErr
	}()

	require.Eventually(func() bool {
		probe, beginErr := st.DB().BeginTx(ctx, nil)
		if beginErr != nil {
			return false
		}
		defer func() { _ = probe.Rollback() }()
		var id int64
		probeErr := probe.QueryRowContext(ctx,
			`SELECT id FROM organizations WHERE id = $1 FOR UPDATE NOWAIT`,
			survivor.ID).Scan(&id)
		return probeErr != nil
	}, 5*time.Second, 10*time.Millisecond,
		"merge never acquired the survivor lock before waiting on the losing row")

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := st.AddEmploymentContext(ctx, store.EmploymentInput{
			PersonID: person.ID, OrganizationID: losing.ID,
			Title: new("Engineer"), Source: store.ProvenanceUser,
		})
		writeDone <- writeErr
	}()
	time.Sleep(100 * time.Millisecond)
	require.NoError(blocker.Commit())

	select {
	case mergeErr := <-mergeDone:
		require.NoError(mergeErr)
	case <-time.After(5 * time.Second):
		require.FailNow("merge did not finish after releasing the losing row")
	}
	select {
	case writeErr := <-writeDone:
		require.ErrorIs(writeErr, store.ErrOrganizationInvalid)
	case <-time.After(5 * time.Second):
		require.FailNow("employment write did not finish after merge")
	}

	employments, err := st.ListEmploymentsContext(
		ctx, store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Empty(employments)
}

func TestDeletingAPersonRemovesTheirEmploymentsButNotTheOrganization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")
	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), Source: store.ProvenanceUser})
	require.NoError(err)
	require.NoError(st.DeletePerson(person.ID, person.Revision))
	remaining, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{OrganizationID: organization.ID})
	require.NoError(err)
	assert.Empty(remaining)
	_, err = st.GetOrganizationContext(ctx, organization.ID)
	require.NoError(err)
}

func TestUpdateAndDeleteEmploymentEnforceRevisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")
	employment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), Source: store.ProvenanceUser})
	require.NoError(err)
	updated, err := st.UpdateEmploymentContext(ctx, employment.ID, employment.Revision, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Senior Engineer"), Department: new("Archive Platform"), Source: store.ProvenanceUser, IsPrimary: new(true)})
	require.NoError(err)
	require.NotNil(updated.Title)
	assert.Equal("Senior Engineer", *updated.Title)
	assert.Equal(employment.Revision+1, updated.Revision)
	_, err = st.UpdateEmploymentContext(ctx, employment.ID, employment.Revision, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Source: store.ProvenanceUser})
	require.Error(err)
	require.ErrorIs(err, store.ErrEmploymentRevisionConflict)
	err = st.DeleteEmploymentContext(ctx, employment.ID, employment.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrEmploymentRevisionConflict)
	require.NoError(st.DeleteEmploymentContext(ctx, employment.ID, updated.Revision))
	_, err = st.GetEmploymentContext(ctx, employment.ID)
	require.Error(err)
	require.ErrorIs(err, store.ErrEmploymentNotFound)
}

func TestUpdateEmploymentClassifiesAnActiveTitleConflict(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")

	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	second, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Advisor"), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	_, err = st.UpdateEmploymentContext(ctx, second.ID, second.Revision, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
		IsPrimary: new(false),
	})
	require.Error(err)
	require.ErrorIs(err, store.ErrEmploymentDuplicateActive)
}

func TestMergeOrganizationsClearsRepointedEmploymentAddressAndBumpsRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	survivor := mustOrganization(t, st, "Example Org")
	losing := mustOrganization(t, st, "Another Org")

	var addressID int64
	err := st.DB().QueryRowContext(ctx, st.Rebind(`
		INSERT INTO organization_addresses (organization_id, original_value, source)
		VALUES (?, ?, ?) RETURNING id
	`), losing.ID, "Synthetic address", string(store.ProvenanceUser)).Scan(&addressID)
	require.NoError(err)

	employment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: losing.ID,
		Title: new("Engineer"), AddressID: &addressID,
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.NotNil(employment.AddressID)

	_, err = st.MergeOrganizationsContext(ctx,
		survivor.ID, survivor.Revision, losing.ID, losing.Revision)
	require.NoError(err)

	got, err := st.GetEmploymentContext(ctx, employment.ID)
	require.NoError(err)
	assert.Equal(survivor.ID, got.OrganizationID)
	assert.Nil(got.AddressID)
	assert.Equal(employment.Revision+1, got.Revision)
}

func mustOrganization(t *testing.T, st *store.Store, name string) *store.Organization {
	t.Helper()
	organization, err := st.CreateOrganizationContext(context.Background(), store.OrganizationInput{Name: name, Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	return organization
}
func mustPartialDate(t *testing.T, raw string) *store.PartialDate {
	t.Helper()
	date, err := store.ParsePartialDate(raw)
	require.NoError(t, err)
	return &date
}
func employmentIDs(employments []store.Employment) []int64 {
	ids := make([]int64, 0, len(employments))
	for _, employment := range employments {
		ids = append(ids, employment.ID)
	}
	return ids
}
func mustPromotedPerson(t *testing.T, st *store.Store, email, name string) *store.Person {
	t.Helper()
	participant, err := st.EnsureParticipant(email, name, "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return person
}

func TestUpdateEmploymentDoesNotPromoteToPrimaryWhenFlagOmitted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "no-promote@example.com", "no-promote")
	firstOrg := mustOrganization(t, st, "Omitted Flag A")
	secondOrg := mustOrganization(t, st, "Omitted Flag B")
	first, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: firstOrg.ID, Title: new("Engineer"), Source: store.ProvenanceUser, IsPrimary: new(false)})
	require.NoError(err)
	second, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: secondOrg.ID, Title: new("Advisor"), Source: store.ProvenanceUser, IsPrimary: new(false)})
	require.NoError(err)

	updated, err := st.UpdateEmploymentContext(ctx, second.ID, second.Revision, store.EmploymentInput{PersonID: person.ID, OrganizationID: secondOrg.ID, Title: new("Senior Advisor"), Source: store.ProvenanceUser})
	require.NoError(err)
	assert.False(updated.IsPrimary, "an update without is_primary must not promote as a side effect")

	promoted, err := st.SetPrimaryEmploymentContext(ctx, first.ID, first.Revision)
	require.NoError(err)
	require.True(promoted.IsPrimary)
	kept, err := st.UpdateEmploymentContext(ctx, promoted.ID, promoted.Revision, store.EmploymentInput{PersonID: person.ID, OrganizationID: firstOrg.ID, Title: new("Staff Engineer"), Source: store.ProvenanceUser})
	require.NoError(err)
	assert.True(kept.IsPrimary, "an update without is_primary must keep the stored flag")

	demoted, err := st.UpdateEmploymentContext(ctx, kept.ID, kept.Revision, store.EmploymentInput{PersonID: person.ID, OrganizationID: firstOrg.ID, Title: new("Staff Engineer"), EndDate: mustPartialDate(t, "2024-05"), IsCurrent: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	assert.False(demoted.IsCurrent)
	assert.False(demoted.IsPrimary, "leaving current status must drop the primary flag")
}

func TestEmploymentRejectsCurrentWithEndDate(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "current-end@example.com", "current-end")
	organization := mustOrganization(t, st, "Current End Org")
	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), EndDate: mustPartialDate(t, "2020-01"), IsCurrent: new(true), Source: store.ProvenanceUser})
	require.Error(err)
	require.ErrorIs(err, store.ErrEmploymentInvalid)
}

func TestConcurrentEmploymentWritesRetryOnSQLiteBusy(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("exercises the SQLite snapshot-upgrade retry path")
	}
	person := mustPromotedPerson(t, st, "busy-retry@example.com", "busy-retry")
	organizations := make([]*store.Organization, 4)
	for i := range organizations {
		organizations[i] = mustOrganization(t, st, fmt.Sprintf("Busy Retry Org %d", i))
	}
	errs := make(chan error, len(organizations))
	for i, organization := range organizations {
		go func() {
			_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new(fmt.Sprintf("Engineer %d", i)), Source: store.ProvenanceUser, IsPrimary: new(false)})
			errs <- err
		}()
	}
	for range organizations {
		require.NoError(<-errs)
	}
	all, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(all, len(organizations))
}

func TestUpdateEmploymentPreservesHistoricalStateWhenFlagsOmitted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "stay-historical@example.com", "stay-historical")
	organization := mustOrganization(t, st, "Stay Historical Org")
	past, err := st.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Engineer"), StartDate: mustPartialDate(t, "2015"), EndDate: mustPartialDate(t, "2018-06"), IsCurrent: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	require.False(past.IsCurrent)

	updated, err := st.UpdateEmploymentContext(ctx, past.ID, past.Revision, store.EmploymentInput{PersonID: person.ID, OrganizationID: organization.ID, Title: new("Senior Engineer"), Source: store.ProvenanceUser})
	require.NoError(err)
	assert.False(updated.IsCurrent,
		"an update that omits is_current and end_date must not resurrect a historical employment")
	assert.False(updated.IsPrimary)
}
