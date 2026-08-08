package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonCategoriesAreOneRowPerTagWithHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	for _, tag := range []string{"Friends", "Book Club"} {
		_, err := st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
			OriginalValue: tag, Envelope: store.ValueEnvelope{Source: store.ProvenanceVCardImport},
		})
		require.NoError(err)
	}
	_, err := st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: "friends", Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonCategoryDuplicate)
	categories, err := st.ListPersonCategoriesContext(ctx, personID, true)
	require.NoError(err)
	require.Len(categories, 2)
	require.NoError(st.SupersedePersonCategoryContext(ctx, personID, categories[1].Envelope.ID, nil))
	_, err = st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: categories[1].OriginalValue,
		Envelope:      store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	all, err := st.ListPersonCategoriesContext(ctx, personID, false)
	require.NoError(err)
	assert.Len(all, 3)
}

func TestAddPersonCategoryRejectsBlankValue(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	personID := newTestPerson(t, st)
	_, err := st.AddPersonCategoryContext(context.Background(), personID, store.PersonCategoryInput{
		OriginalValue: "   ", Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonCategoryEmpty)
}
