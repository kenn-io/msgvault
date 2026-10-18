package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestScopedPersonAttributeAbsentSlotCAS(t *testing.T) {
	for _, cardinality := range []store.AttributeCardinality{store.AttributeCardinalitySingle, store.AttributeCardinalityMulti} {
		t.Run(string(cardinality), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := storetest.New(t).Store
			person := mustPromotedPerson(t, st, "attribute-scope@example.test", "Synthetic Attribute")
			definition := personTextDefinition("synthetic_scoped_value")
			definition.Cardinality = cardinality
			_, err := st.CreateAttributeDefinitionContext(t.Context(), definition)
			requirements.NoError(err)
			input := store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Synthetic Value")}, Source: store.ProvenanceUser, ExpectedValueID: new(int64(0))}
			if cardinality == store.AttributeCardinalityMulti {
				_, err := st.SetPersonAttributeValueContext(t.Context(), input)
				requirements.ErrorIs(err, store.ErrAttributeValueInvalid, "an absent multi-value slot requires its ordinal")
				input.Ordinal = new(int64(2))
			}
			denied := errors.New("synthetic scope denied")
			_, err = st.SetPersonAttributeValueAuthorizedContext(t.Context(), input, func(context.Context, *store.IdentityGrantSelection) error { return denied })
			requirements.ErrorIs(err, denied)
			values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
			requirements.NoError(err)
			assertions.Empty(values)
			write, err := st.SetPersonAttributeValueAuthorizedContext(t.Context(), input, func(context.Context, *store.IdentityGrantSelection) error { return nil })
			requirements.NoError(err)
			requirements.NotNil(write.Value)
			_, err = st.SetPersonAttributeValueAuthorizedContext(t.Context(), input, func(context.Context, *store.IdentityGrantSelection) error { return nil })
			requirements.ErrorIs(err, store.ErrAttributeValueConflict)
			_, err = st.SupersedePersonAttributeValueAuthorizedContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: definition.Slug, Ordinal: input.Ordinal, ExpectedValueID: &write.Value.ID}, func(context.Context, *store.IdentityGrantSelection) error { return denied })
			requirements.ErrorIs(err, denied)
			values, err = st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
			requirements.NoError(err)
			requirements.Len(values, 1)
			assertions.Equal(write.Value.ID, values[0].ID)
		})
	}
}

// A one-connection pool cannot serve a nested transaction. The timeout bounds
// that observable deadlock; authorization must use the already open transaction.
func TestPersonAuthorizationUsesExistingTransaction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := storetest.New(t).Store
	person := mustPromotedPerson(t, st, "single-connection@example.test", "Synthetic Transaction")
	st.DB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	seen := false
	updated, err := st.UpdatePersonDisplayNameAuthorizedContext(ctx, person.ID, person.Revision, new("Synthetic Updated"), func(_ context.Context, scope *store.IdentityGrantSelection) error {
		seen = true
		requirements.Len(scope.Persons, 1)
		assertions.Equal(person.VCardUID, scope.Persons[0].UID)
		return nil
	})
	requirements.NoError(err)
	assertions.True(seen)
	assertions.Equal(new("Synthetic Updated"), updated.DisplayName)
}
