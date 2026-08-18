package vcardmap_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcardmap"
)

// TestStoreProjectionMapsToOrgTitleAndRole locks the PR 5 hook that roadmap
// PR 8 consumes: the derived primary current employment, and only that row,
// supplies vCard ORG, TITLE, and ROLE.
func TestStoreProjectionMapsToOrgTitleAndRole(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	participantID, err := st.EnsureParticipant("alice@example.com", "alice", "example.com")
	require.NoError(err)
	person, created, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	require.True(created)

	dayJob, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	sideJob, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Another Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	title := "Staff Engineer"
	role := "Engineering"
	department := "Archive Platform"
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: dayJob.ID,
		Title: &title, Role: &role, Department: &department,
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	advisorTitle := "Advisor"
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: sideJob.ID,
		Title: &advisorTitle, Source: store.ProvenanceUser,
	})
	require.NoError(err)

	projection, found, err := st.PrimaryCurrentEmploymentContext(ctx, person.ID)
	require.NoError(err)
	require.True(found)

	employment := vcardmap.FromProjection(projection)
	assert.Equal([]string{"Example Org", "Archive Platform"},
		vcardmap.OrgComponents(employment))
	assert.Equal("Staff Engineer", vcardmap.Title(employment))
	assert.Equal("Engineering", vcardmap.Role(employment))
}

func TestNoPrimaryCurrentEmploymentYieldsNoOrgValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	participantID, err := st.EnsureParticipant("bob@example.com", "bob", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)

	projection, found, err := st.PrimaryCurrentEmploymentContext(ctx, person.ID)
	require.NoError(err)
	require.False(found)

	employment := vcardmap.FromProjection(projection)
	assert.Nil(vcardmap.OrgComponents(employment))
	assert.Empty(vcardmap.Title(employment))
	assert.Empty(vcardmap.Role(employment))
}
