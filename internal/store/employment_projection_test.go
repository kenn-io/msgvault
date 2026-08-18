package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPrimaryCurrentEmploymentProjectsCompanyAndTitle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")

	_, found, err := st.PrimaryCurrentEmploymentContext(ctx, person.ID)
	require.NoError(err)
	assert.False(found, "a person with no employment has no projection")

	employment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID:       person.ID,
		OrganizationID: organization.ID,
		Title:          new("Staff Engineer"),
		Role:           new("Engineering"),
		Department:     new("Archive Platform"),
		Source:         store.ProvenanceUser,
	})
	require.NoError(err)

	projection, found, err := st.PrimaryCurrentEmploymentContext(ctx, person.ID)
	require.NoError(err)
	require.True(found)
	assert.Equal(store.EmploymentProjection{
		PersonID:         person.ID,
		EmploymentID:     employment.ID,
		OrganizationID:   organization.ID,
		OrganizationName: "Example Org",
		Title:            "Staff Engineer",
		Role:             "Engineering",
		Department:       "Archive Platform",
	}, projection)
}

func TestEndingThePrimaryEmploymentClearsTheProjectionWithoutDeletingHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	organization := mustOrganization(t, st, "Example Org")

	employment, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Staff Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	endDate, err := store.ParsePartialDate("2026-06")
	require.NoError(err)
	_, err = st.EndEmploymentContext(ctx, employment.ID, employment.Revision, endDate)
	require.NoError(err)

	_, found, err := st.PrimaryCurrentEmploymentContext(ctx, person.ID)
	require.NoError(err)
	assert.False(found, "the projection follows the primary current row")

	history, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(history, 1, "history is intact after the projection clears")
	assert.Equal(employment.ID, history[0].ID)
}

func TestPrimaryCurrentEmploymentFollowsRotation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	person := mustPromotedPerson(t, st, "alice@example.com", "alice")
	dayJob := mustOrganization(t, st, "Example Org")
	sideJob := mustOrganization(t, st, "Another Org")

	_, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: dayJob.ID,
		Title: new("Staff Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	side, err := st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: sideJob.ID,
		Title: new("Advisor"), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	_, err = st.SetPrimaryEmploymentContext(ctx, side.ID, side.Revision)
	require.NoError(err)

	projection, found, err := st.PrimaryCurrentEmploymentContext(ctx, person.ID)
	require.NoError(err)
	require.True(found)
	assert.Equal("Another Org", projection.OrganizationName)
	assert.Equal("Advisor", projection.Title)
}
