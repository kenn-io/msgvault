package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCreateGetAndListOrganizations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	description := "Synthetic fixture organization."
	domain := "Example.COM"
	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "  Example Org  ", Kind: store.OrganizationKindCompany,
		PrimaryDomain: &domain, Description: &description,
	})
	require.NoError(err)
	assert.Positive(created.ID)
	assert.Equal("Example Org", created.Name)
	assert.Equal(store.OrganizationKindCompany, created.Kind)
	require.NotNil(created.PrimaryDomain)
	assert.Equal("example.com", *created.PrimaryDomain)
	assert.Equal(int64(1), created.Revision)
	assert.Nil(created.RetiredAt)
	assert.Nil(created.MergedIntoID)

	got, err := st.GetOrganizationContext(ctx, created.ID)
	require.NoError(err)
	assert.Equal(created.ID, got.ID)
	assert.Equal(created.Revision, got.Revision)

	second, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Another Org", Kind: store.OrganizationKindNonprofit,
	})
	require.NoError(err)

	listed, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{Limit: 10})
	require.NoError(err)
	require.Len(listed, 2)
	assert.Equal(second.ID, listed[0].ID)
	assert.Equal(created.ID, listed[1].ID)

	total, err := st.CountOrganizationsContext(ctx, store.OrganizationFilter{})
	require.NoError(err)
	assert.Equal(int64(2), total)

	filtered, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{
		Query: "exam", Limit: 10,
	})
	require.NoError(err)
	require.Len(filtered, 1)
	assert.Equal(created.ID, filtered[0].ID)
}

func TestCreateOrganizationRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	tests := []struct {
		name    string
		input   store.OrganizationInput
		wantErr string
	}{
		{
			name: "empty name",
			input: store.OrganizationInput{
				Name: "   ", Kind: store.OrganizationKindCompany,
			},
			wantErr: "name is required",
		},
		{
			name: "unknown kind",
			input: store.OrganizationInput{
				Name: "Example Org", Kind: "conglomerate",
			},
			wantErr: `unknown kind "conglomerate"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, err := st.CreateOrganizationContext(ctx, test.input)
			require.Error(err)
			require.ErrorIs(err, store.ErrOrganizationInvalid)
			assert.ErrorContains(err, test.wantErr)
		})
	}
}

func TestCreateOrganizationDefaultsKindToOther(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(
		context.Background(), store.OrganizationInput{Name: "Example Org"})
	require.NoError(err)
	assert.Equal(store.OrganizationKindOther, created.Kind)
}

func TestUpdateOrganizationBumpsRevisionAndRejectsStaleWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	updated, err := st.ReplaceOrganizationContext(ctx, created.ID, created.Revision,
		store.OrganizationInput{Name: "Example Group", Kind: store.OrganizationKindCompany}, false)
	require.NoError(err)
	assert.Equal("Example Group", updated.Name)
	assert.Equal(created.Revision+1, updated.Revision)

	_, err = st.ReplaceOrganizationContext(ctx, created.ID, created.Revision,
		store.OrganizationInput{Name: "Stale Write", Kind: store.OrganizationKindCompany}, false)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationRevisionConflict)

	_, err = st.ReplaceOrganizationContext(ctx, created.ID+9999, 1,
		store.OrganizationInput{Name: "Missing", Kind: store.OrganizationKindCompany}, false)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationNotFound)
}

func TestRetireAndUnretireOrganizationHidesItFromDefaultListing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	retired, err := st.ReplaceOrganizationContext(ctx, created.ID, created.Revision,
		store.OrganizationInput{Name: "Example Org", Kind: store.OrganizationKindCompany}, true)
	require.NoError(err)
	require.NotNil(retired.RetiredAt)
	assert.Equal(created.Revision+1, retired.Revision)

	listed, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{Limit: 10})
	require.NoError(err)
	assert.Empty(listed)

	withRetired, err := st.ListOrganizationsContext(ctx, store.OrganizationFilter{
		IncludeRetired: true, Limit: 10,
	})
	require.NoError(err)
	require.Len(withRetired, 1)
	assert.Equal(created.ID, withRetired[0].ID)

	revived, err := st.ReplaceOrganizationContext(ctx, created.ID, retired.Revision,
		store.OrganizationInput{Name: "Example Org", Kind: store.OrganizationKindCompany}, false)
	require.NoError(err)
	assert.Nil(revived.RetiredAt)
	assert.Equal(retired.Revision+1, revived.Revision)
}

func TestDeleteOrganizationSucceedsOnlyWithoutEmploymentHistory(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	created, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	require.NoError(st.DeleteOrganizationContext(ctx, created.ID, created.Revision))

	_, err = st.GetOrganizationContext(ctx, created.ID)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationNotFound)

	err = st.DeleteOrganizationContext(ctx, created.ID, created.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationNotFound)
}

func TestNormalizeOrganizationNameAndDomain(t *testing.T) {
	assert := assert.New(t)
	nameTests := []struct{ raw, want string }{
		{raw: "Example Org", want: "example org"},
		{raw: "  Example   Org  ", want: "example org"},
		{raw: "EXAMPLE\tORG", want: "example org"},
		{raw: "", want: ""},
	}
	for _, test := range nameTests {
		assert.Equal(test.want, store.NormalizeOrganizationName(test.raw), "raw %q", test.raw)
	}

	domainTests := []struct{ raw, want string }{
		{raw: "Example.COM", want: "example.com"},
		{raw: "  WWW.Example.com  ", want: "example.com"},
		{raw: "https://example.com/careers", want: "example.com"},
		{raw: "user@example.com", want: "example.com"},
		{raw: "", want: ""},
	}
	for _, test := range domainTests {
		assert.Equal(test.want, store.NormalizeDomain(test.raw), "raw %q", test.raw)
	}
}

func TestNormalizeDomainMatchesStoreNormalization(t *testing.T) {
	for _, raw := range []string{
		"Example.COM", "https://www.bücher.example/jobs", "person@BÜCHER.example",
		"https://www.Example.com/search?q=user@other.example",
		"https://user:secret@www.Example.com/path",
		"", "localhost", "example.com/path", "https://",
	} {
		t.Run(raw, func(t *testing.T) {
			shared, err := personfacts.NormalizeDomain(raw)
			if err != nil {
				assert.Empty(t, store.NormalizeDomain(raw))
				return
			}
			assert.Equal(t, shared, store.NormalizeDomain(raw))
		})
	}
}

func TestMergeOrganizationsRejectsSelfWithoutChangingTheRoot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	_, err = st.MergeOrganizationsContext(ctx,
		organization.ID, organization.Revision, organization.ID, organization.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	require.ErrorContains(err, "cannot merge an organization into itself")

	unchanged, err := st.GetOrganizationContext(ctx, organization.ID)
	require.NoError(err)
	assert.Equal(organization.Revision, unchanged.Revision)
	assert.Nil(unchanged.MergedIntoID)
}

func TestMergedOrganizationRedirectCannotBeRemergedOrDeleted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	firstSurvivor := mustOrganization(t, st, "Example Org")
	secondSurvivor := mustOrganization(t, st, "Another Org")
	losing := mustOrganization(t, st, "Former Org")

	_, err := st.MergeOrganizationsContext(ctx,
		firstSurvivor.ID, firstSurvivor.Revision, losing.ID, losing.Revision)
	require.NoError(err)
	redirect, err := st.GetOrganizationContext(ctx, losing.ID)
	require.NoError(err)
	require.NotNil(redirect.MergedIntoID)

	_, err = st.MergeOrganizationsContext(ctx,
		secondSurvivor.ID, secondSurvivor.Revision, redirect.ID, redirect.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationInvalid)

	err = st.DeleteOrganizationContext(ctx, redirect.ID, redirect.Revision)
	require.Error(err)
	require.ErrorIs(err, store.ErrOrganizationInvalid)

	unchanged, err := st.GetOrganizationContext(ctx, redirect.ID)
	require.NoError(err)
	assert.Equal(redirect.Revision, unchanged.Revision)
	require.NotNil(unchanged.MergedIntoID)

	survivor, err := st.GetOrganizationContext(ctx, firstSurvivor.ID)
	require.NoError(err)
	err = st.DeleteOrganizationContext(ctx, survivor.ID, survivor.Revision)
	require.Error(err,
		"deleting a merge survivor must fail with a typed error, not a raw FK violation")
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	require.ErrorContains(err, "redirect")
	assert.Equal(firstSurvivor.ID, *unchanged.MergedIntoID)
}

func TestMergedOrganizationRedirectRejectsRootAndProfileMutations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *store.Store, *store.Organization) error
	}{
		{
			name: "root update",
			mutate: func(ctx context.Context, st *store.Store, redirect *store.Organization) error {
				_, err := st.ReplaceOrganizationContext(
					ctx, redirect.ID, redirect.Revision,
					store.OrganizationInput{
						Name: "Hidden Rewrite", Kind: store.OrganizationKindCompany,
					}, false)
				return err
			},
		},
		{
			name: "retire",
			mutate: func(ctx context.Context, st *store.Store, redirect *store.Organization) error {
				_, err := st.ReplaceOrganizationContext(
					ctx, redirect.ID, redirect.Revision,
					store.OrganizationInput{
						Name: "Redirect Retire", Kind: store.OrganizationKindCompany,
					}, true)
				return err
			},
		},
		{
			name: "profile replacement",
			mutate: func(ctx context.Context, st *store.Store, redirect *store.Organization) error {
				_, err := st.ReplaceOrganizationProfileContext(
					ctx, redirect.ID, redirect.Revision,
					store.OrganizationProfileInput{})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := context.Background()
			st := testutil.NewTestStore(t)
			survivor := mustOrganization(t, st, "Immutable Survivor")
			losing := mustOrganization(t, st, "Immutable Redirect")
			_, err := st.MergeOrganizationsContext(ctx,
				survivor.ID, survivor.Revision, losing.ID, losing.Revision)
			require.NoError(err)
			redirect, err := st.GetOrganizationContext(ctx, losing.ID)
			require.NoError(err)

			err = test.mutate(ctx, st, redirect)
			require.ErrorIs(err, store.ErrOrganizationInvalid)

			unchanged, err := st.GetOrganizationContext(ctx, redirect.ID)
			require.NoError(err)
			assert.Equal(redirect.Revision, unchanged.Revision)
			assert.Equal(redirect.Name, unchanged.Name)
			assert.Equal(redirect.RetiredAt, unchanged.RetiredAt)
		})
	}
}

func TestOrganizationKindVocabularyIsOpenAtTheDatabaseBoundary(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	var organizationID int64
	err := st.DB().QueryRowContext(ctx, st.Rebind(`
		INSERT INTO organizations (name, name_normalized, kind)
		VALUES (?, ?, ?)
		RETURNING id
	`), "Open Vocabulary Cooperative", "open vocabulary cooperative",
		"cooperative").Scan(&organizationID)
	require.NoError(err)
	require.Positive(organizationID)
}

func TestOrganizationNameKindVocabularyIsOpenAtTheDatabaseBoundary(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	organization := mustOrganization(t, st, "Open Vocabulary Names")

	result, err := st.DB().ExecContext(ctx, st.Rebind(`
		INSERT INTO organization_names (
			organization_id, name_kind, original_value, name_normalized, source
		) VALUES (?, ?, ?, ?, ?)
	`), organization.ID, "localized", "Nom localisé", "nom localisé",
		string(store.ProvenanceUser))
	require.NoError(err)
	affected, err := result.RowsAffected()
	require.NoError(err)
	require.Equal(int64(1), affected)
}

func TestMergeRetiresLosingProfileValuesAndAttributes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	survivor, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Survivor Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	losing, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Losing Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	losingProfile, err := st.ReplaceOrganizationProfileContext(
		ctx, losing.ID, losing.Revision, store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{{
				Name: "Losing Organisation", NameKind: store.OrganizationNameKindAlias,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Identifiers: []store.OrganizationIdentifierInput{{
				IdentifierKind: store.OrganizationIdentifierKindDomain,
				Value:          "losing.example.com",
				Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		})
	require.NoError(err)
	require.Len(losingProfile.Names, 1)

	contactParticipant, err := st.EnsureParticipant(
		"merge-contact@example.com", "merge-contact", "example.com")
	require.NoError(err)
	contact, _, err := st.CreatePersonFromParticipant(contactParticipant)
	require.NoError(err)
	definition := organizationTextDefinition("merge_primary_contact")
	definition.ValueType = store.AttributeValueRecordReference
	definition.FieldType = store.AttributeFieldPerson
	definition.RecordTarget = new("person")
	_, err = st.CreateAttributeDefinitionContext(ctx, definition)
	require.NoError(err)
	_, err = st.SetOrganizationAttributeValueContext(ctx, store.OrganizationAttributeValueInput{
		OrganizationID: losing.ID, DefinitionSlug: definition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueRecordReference, RecordType: new("person"),
			RecordID: &contact.ID,
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	_, err = st.MergeOrganizationsContext(ctx,
		survivor.ID, survivor.Revision, losing.ID, losingProfile.Organization.Revision)
	require.NoError(err)

	activeProfile, err := st.GetOrganizationProfileContext(ctx, losing.ID, false)
	require.NoError(err)
	assert.Empty(activeProfile.Names, "merged redirect must not keep active names")
	assert.Empty(activeProfile.Identifiers, "merged redirect must not keep active identifiers")
	history, err := st.GetOrganizationProfileContext(ctx, losing.ID, true)
	require.NoError(err)
	assert.NotEmpty(history.Names, "superseded rows must remain readable as history")

	values, err := st.ListOrganizationAttributeValuesContext(ctx, losing.ID,
		store.OrganizationAttributeQuery{})
	require.NoError(err)
	assert.Empty(values, "merged redirect must not keep active attribute values")

	err = st.DeletePersonContext(ctx, contact.ID, contact.Revision)
	require.NoError(err,
		"a superseded reference on a merged redirect must not block person deletion")
}

// TestOrganizationReplacementRetriesEmploymentDeadlock forces the lock cycle
// between organization replacement (organization row, then the rows of the
// people employed there) and an employment write (person row, then the
// employer row). The blocker plays the employment writer: it holds the person
// and asks for the organization once the replacement is parked on the person.
// PostgreSQL's detector aborts one side; the replacement has to absorb that
// and finish once the blocker lets go.
func TestOrganizationReplacementRetriesEmploymentDeadlock(t *testing.T) {
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for the replacement retry regression")
	}
	cases := []struct {
		name    string
		replace func(ctx context.Context, organization *store.Organization) error
	}{{
		name: "root fields",
		replace: func(ctx context.Context, organization *store.Organization) error {
			_, err := st.ReplaceOrganizationContext(ctx, organization.ID, organization.Revision,
				store.OrganizationInput{Name: "Example Group", Kind: store.OrganizationKindCompany}, false)
			return err
		},
	}, {
		name: "profile",
		replace: func(ctx context.Context, organization *store.Organization) error {
			_, err := st.ReplaceOrganizationProfileContext(ctx, organization.ID, organization.Revision,
				store.OrganizationProfileInput{
					Media: []store.OrganizationMediaInput{{
						MediaKind: store.PersonMediaLogo,
						URI:       new("https://example.test/logo.png"),
						Envelope:  store.ValueEnvelopeInput{Source: store.ProvenanceUser},
					}},
				})
			return err
		},
	}}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
				Name: fmt.Sprintf("Example Org %d", i), Kind: store.OrganizationKindCompany,
			})
			require.NoError(err)
			person := createEnvelopePerson(t, st, fmt.Sprintf("employee%d@example.com", i))
			_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
				PersonID: person.ID, OrganizationID: organization.ID,
				Title: new("Engineer"), Source: store.ProvenanceUser,
			})
			require.NoError(err)

			replaceErr := forcePostgreSQLDeadlock(ctx, t, st,
				postgreSQLRowLock{table: "persons", id: person.ID},
				postgreSQLRowLock{table: "organizations", id: organization.ID},
				func(ctx context.Context) error { return tc.replace(ctx, organization) })
			require.NoError(replaceErr, "organization replacement must retry a transient PostgreSQL deadlock")
			replaced, err := st.GetOrganizationContext(ctx, organization.ID)
			require.NoError(err)
			require.Equal(organization.Revision+1, replaced.Revision)
		})
	}
}
