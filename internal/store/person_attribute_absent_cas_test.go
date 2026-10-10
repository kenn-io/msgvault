package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonAttributeValueCASCanRequireAbsentExplicitSlot(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("attribute-cas@example.test", "Attribute CAS Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), personTextDefinition("synthetic_cas"))
	requirements.NoError(err)
	input := store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Synthetic CAS value")}, Source: store.ProvenanceUser, ExpectedValueID: new(int64(0))}
	first, err := st.SetPersonAttributeValueContext(t.Context(), input)
	requirements.NoError(err)
	requirements.NotNil(first.Value)
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	requirements.ErrorIs(err, store.ErrAttributeValueConflict)
	current, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
	requirements.NoError(err)
	requirements.Len(current, 1)
	assertions.Equal(first.Value.ID, current[0].ID)
	input.ExpectedValueID = &first.Value.ID
	input.Value.Text = new("Updated synthetic CAS value")
	updated, err := st.SetPersonAttributeValueContext(t.Context(), input)
	requirements.NoError(err)
	requirements.NotNil(updated.Superseded)
	assertions.Equal(first.Value.ID, updated.Superseded.ID)

	multiple := personTextDefinition("synthetic_cas_multiple")
	multiple.Cardinality = store.AttributeCardinalityMulti
	multi, err := st.CreateAttributeDefinitionContext(t.Context(), multiple)
	requirements.NoError(err)
	input.DefinitionSlug = multi.Slug
	input.ExpectedValueID = new(int64(0))
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	requirements.ErrorIs(err, store.ErrAttributeValueInvalid)
	input.Ordinal = new(int64(2))
	firstMulti, err := st.SetPersonAttributeValueContext(t.Context(), input)
	requirements.NoError(err)
	requirements.NotNil(firstMulti.Value)
	assertions.Equal(int64(2), firstMulti.Value.Ordinal)
	_, err = st.SetPersonAttributeValueContext(t.Context(), input)
	requirements.ErrorIs(err, store.ErrAttributeValueConflict)
}
