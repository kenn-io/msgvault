package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Person-only grants cannot authorize mutations of organization-owned history.
func TestPersonMergeAuthorizationRefusesOrganizationReferences(t *testing.T) {
	for _, historical := range []bool{false, true} {
		name := "current"
		if historical {
			name = "historical"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := storetest.New(t).Store
			survivor := mustPromotedPerson(t, st, "org-survivor@example.test", "Synthetic Survivor")
			absorbed := mustPromotedPerson(t, st, "org-absorbed@example.test", "Synthetic Absorbed")
			organization := mustAttributeOrganization(t, st)
			definition := personTextDefinition("synthetic_organization_reference")
			definition.UniversalID = "synthetic-org-merge-reference"
			definition.ObjectType = store.AttributeObjectOrganization
			definition.ValueType = store.AttributeValueRecordReference
			definition.FieldType = store.AttributeFieldPerson
			definition.RecordTarget = new("person")
			_, err := st.CreateAttributeDefinitionContext(t.Context(), definition)
			requirements.NoError(err)
			value, err := st.SetOrganizationAttributeValueContext(t.Context(), store.OrganizationAttributeValueInput{OrganizationID: organization.ID, DefinitionSlug: definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: new("person"), RecordID: &absorbed.ID}, Source: store.ProvenanceUser})
			requirements.NoError(err)
			if historical {
				_, err = st.SupersedeOrganizationAttributeValueContext(t.Context(), store.OrganizationAttributeSupersedeInput{OrganizationID: organization.ID, DefinitionSlug: definition.Slug, ExpectedValueID: &value.Value.ID})
				requirements.NoError(err)
			}
			before, err := st.ListOrganizationAttributeValuesContext(t.Context(), organization.ID, store.OrganizationAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
			requirements.NoError(err)
			request := store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-org-scope-merge", Actor: "agent:synthetic-grant"}
			called := false
			_, err = st.MergePersonsAuthorizedContext(t.Context(), request, func(context.Context, *store.IdentityGrantSelection) error { called = true; return nil })
			requirements.ErrorContains(err, "organization")
			assertions.False(called, "a person-only selection cannot admit organization effects")
			after, err := st.ListOrganizationAttributeValuesContext(t.Context(), organization.ID, store.OrganizationAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
			requirements.NoError(err)
			assertions.Equal(before, after)
			_, err = st.GetPerson(absorbed.ID)
			requirements.NoError(err)
			_, err = st.MergePersonsContext(t.Context(), request)
			requirements.NoError(err, "native owner merge retains its existing organization reference handling")
			after, err = st.ListOrganizationAttributeValuesContext(t.Context(), organization.ID, store.OrganizationAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
			requirements.NoError(err)
			requirements.Len(after, 1)
			assertions.Equal(new(survivor.ID), after[0].Value.RecordID)
		})
	}
}
